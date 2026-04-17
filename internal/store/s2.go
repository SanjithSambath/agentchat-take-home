// Package store — concrete S2 implementation.
//
// This file implements the S2Store, AppendSession, and ReadSession interfaces
// declared in interfaces.go. It is a thin wrapper around
// github.com/s2-streamstore/s2-sdk-go that:
//
//   - hides every SDK type behind the frozen store.* contracts
//   - maps SDK errors to store sentinels (ErrRangeNotSatisfiable, ErrSlowWriter)
//   - turns the SDK's two-phase Submit → Wait → Ack pattern into the single
//     non-blocking Submit + background drain that the handler layer expects
//   - surfaces a background ack failure via a sticky cached error on Submit,
//     so a poisoned session stops accepting writes immediately
//
// See ALL_DESIGN_IMPLEMENTATION/s2-architecture-plan.md for the rationale.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/s2-streamstore/s2-sdk-go/s2"

	"agentmail/internal/config"
	"agentmail/internal/model"
)

// submitTimeout bounds how long AppendSession.Submit will wait for the SDK's
// capacity-reservation machinery. Exceeding it surfaces as ErrSlowWriter.
const submitTimeout = 2 * time.Second

// submitQueueDepth caps the in-flight ack-drain queue. 256 is ~5 seconds of
// 50 tok/s without blocking Submit. When full, Submit is briefly synchronous,
// which itself will trip the 2 s timeout and surface as ErrSlowWriter.
const submitQueueDepth = 256

// typeHeaderName is the S2 record header that carries model.EventType.
const typeHeaderName = "type"

// ----------------------------------------------------------------------
// Constructor
// ----------------------------------------------------------------------

// NewS2Store constructs an S2Store against the basin named in cfg.S2Basin.
// The basin is assumed to be pre-provisioned with CreateStreamOnAppend=true,
// arrival-mode timestamping, Express storage class, and the target retention.
func NewS2Store(cfg config.Config) (S2Store, error) {
	if cfg.S2AuthToken == "" {
		return nil, errors.New("s2 store: empty S2_AUTH_TOKEN")
	}
	if cfg.S2Basin == "" {
		return nil, errors.New("s2 store: empty S2_BASIN")
	}

	client := s2.New(cfg.S2AuthToken, &s2.ClientOptions{
		Compression: s2.CompressionZstd,
		RetryConfig: &s2.RetryConfig{
			MaxAttempts:       3,
			MinBaseDelay:      100 * time.Millisecond,
			MaxBaseDelay:      1 * time.Second,
			AppendRetryPolicy: s2.AppendRetryPolicyAll,
		},
	})

	return &s2StoreImpl{
		client: client,
		basin:  client.Basin(cfg.S2Basin),
	}, nil
}

// ----------------------------------------------------------------------
// Concrete types
// ----------------------------------------------------------------------

type s2StoreImpl struct {
	client *s2.Client
	basin  *s2.BasinClient
}

func (s *s2StoreImpl) stream(convID uuid.UUID) *s2.StreamClient {
	return s.basin.Stream(s2.StreamName(StreamNameForConversation(convID)))
}

// ----------------------------------------------------------------------
// AppendEvents (unary)
// ----------------------------------------------------------------------

func (s *s2StoreImpl) AppendEvents(ctx context.Context, convID uuid.UUID, events []model.Event) (AppendResult, error) {
	if len(events) == 0 {
		return AppendResult{}, errors.New("s2 store: AppendEvents called with empty batch")
	}

	records := make([]s2.AppendRecord, len(events))
	for i, ev := range events {
		rec, err := eventToRecord(ev)
		if err != nil {
			return AppendResult{}, fmt.Errorf("s2 store: encode event %d: %w", i, err)
		}
		records[i] = rec
	}

	ack, err := s.stream(convID).Append(ctx, &s2.AppendInput{Records: records})
	if err != nil {
		return AppendResult{}, mapS2Err(err)
	}

	return AppendResult{
		StartSeq: ack.Start.SeqNum,
		EndSeq:   ack.End.SeqNum,
	}, nil
}

// ----------------------------------------------------------------------
// OpenAppendSession
// ----------------------------------------------------------------------

func (s *s2StoreImpl) OpenAppendSession(ctx context.Context, convID uuid.UUID) (AppendSession, error) {
	// Session inherits retry + compression from the client via the basin.
	sess, err := s.stream(convID).AppendSession(ctx, nil)
	if err != nil {
		return nil, mapS2Err(err)
	}

	a := &appendSessionImpl{
		session: sess,
		tickets: make(chan *s2.BatchSubmitTicket, submitQueueDepth),
		done:    make(chan struct{}),
		convID:  convID,
	}
	// Use a detached context for background Ack waits so a cancelled caller
	// ctx doesn't poison in-flight acks. The drainer exits when `tickets` is
	// closed (via appendSessionImpl.Close).
	a.drainCtx, a.drainCancel = context.WithCancel(context.Background())

	go a.drainLoop()

	return a, nil
}

type appendSessionImpl struct {
	session *s2.AppendSession
	tickets chan *s2.BatchSubmitTicket
	done    chan struct{} // closed when drainLoop exits

	cachedErr atomic.Pointer[error] // first sticky bg ack failure

	drainCtx    context.Context
	drainCancel context.CancelFunc

	closeOnce sync.Once
	closeErr  error

	convID uuid.UUID
}

func (a *appendSessionImpl) storeErr(err error) {
	if err == nil {
		return
	}
	// Only the first error sticks.
	a.cachedErr.CompareAndSwap(nil, &err)
}

// drainLoop pulls tickets off the queue in the order Submit enqueued them and
// waits for the durable ack. Any failure is cached so future Submit/Close
// calls can surface it. The loop exits when `tickets` is closed.
func (a *appendSessionImpl) drainLoop() {
	defer close(a.done)
	for ticket := range a.tickets {
		if _, err := ticket.Ack(a.drainCtx); err != nil {
			if errors.Is(err, context.Canceled) {
				// Shutdown path; not a user-visible failure.
				continue
			}
			log.Warn().
				Err(err).
				Str("conv_id", a.convID.String()).
				Msg("s2 append session ack failed")
			a.storeErr(err)
		}
	}
}

func (a *appendSessionImpl) Submit(parentCtx context.Context, ev model.Event) error {
	if ep := a.cachedErr.Load(); ep != nil {
		return *ep
	}

	rec, err := eventToRecord(ev)
	if err != nil {
		return fmt.Errorf("s2 store: encode event: %w", err)
	}

	future, err := a.session.Submit(&s2.AppendInput{Records: []s2.AppendRecord{rec}})
	if err != nil {
		if errors.Is(err, s2.ErrSessionClosed) {
			return err
		}
		a.storeErr(err)
		return err
	}

	waitCtx, cancel := context.WithTimeout(parentCtx, submitTimeout)
	defer cancel()

	ticket, err := future.Wait(waitCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ErrSlowWriter
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		a.storeErr(err)
		return err
	}

	select {
	case a.tickets <- ticket:
		return nil
	default:
		// Drainer is >submitQueueDepth behind; this is slow-writer territory.
		// Fall back to a brief blocking enqueue bounded by the same budget.
		enqCtx, cancel := context.WithTimeout(parentCtx, submitTimeout)
		defer cancel()
		select {
		case a.tickets <- ticket:
			return nil
		case <-enqCtx.Done():
			if errors.Is(enqCtx.Err(), context.DeadlineExceeded) {
				return ErrSlowWriter
			}
			return enqCtx.Err()
		}
	}
}

func (a *appendSessionImpl) Close() error {
	a.closeOnce.Do(func() {
		// Stop accepting new tickets; drainer will exit after draining the queue.
		close(a.tickets)
		<-a.done
		a.drainCancel()
		if err := a.session.Close(); err != nil {
			a.closeErr = err
		}
	})
	if ep := a.cachedErr.Load(); ep != nil {
		return *ep
	}
	return a.closeErr
}

// ----------------------------------------------------------------------
// OpenReadSession
// ----------------------------------------------------------------------

func (s *s2StoreImpl) OpenReadSession(ctx context.Context, convID uuid.UUID, fromSeq uint64) (ReadSession, error) {
	sess, err := s.stream(convID).ReadSession(ctx, &s2.ReadOptions{
		SeqNum: s2.Uint64(fromSeq),
	})
	if err != nil {
		return nil, mapS2Err(err)
	}
	return &readSessionImpl{session: sess, convID: convID}, nil
}

type readSessionImpl struct {
	session   *s2.ReadSession
	current   model.SequencedEvent
	err       error
	closeOnce sync.Once
	closed    atomic.Bool
	convID    uuid.UUID
}

func (r *readSessionImpl) Next() bool {
	if r.err != nil || r.closed.Load() {
		return false
	}
	for r.session.Next() {
		rec := r.session.Record()
		if rec.IsCommandRecord() {
			// Command records (fence, trim) are bookkeeping; skip silently.
			continue
		}
		ev, ok := recordToSequencedEvent(rec)
		if !ok {
			log.Warn().
				Uint64("seq_num", rec.SeqNum).
				Str("conv_id", r.convID.String()).
				Msg("s2 read session: dropping record with missing/unknown type header")
			continue
		}
		r.current = ev
		return true
	}
	r.err = mapS2Err(r.session.Err())
	return false
}

func (r *readSessionImpl) Event() model.SequencedEvent {
	return r.current
}

func (r *readSessionImpl) Err() error {
	return r.err
}

func (r *readSessionImpl) Close() error {
	var err error
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		err = r.session.Close()
	})
	return err
}

// ----------------------------------------------------------------------
// ReadRange (bounded, non-tailing)
// ----------------------------------------------------------------------

func (s *s2StoreImpl) ReadRange(ctx context.Context, convID uuid.UUID, fromSeq uint64, limit int) ([]model.SequencedEvent, error) {
	if limit <= 0 {
		return nil, nil
	}
	batch, err := s.stream(convID).Read(ctx, &s2.ReadOptions{
		SeqNum: s2.Uint64(fromSeq),
		Count:  s2.Uint64(uint64(limit)),
	})
	if err != nil {
		return nil, mapS2Err(err)
	}

	out := make([]model.SequencedEvent, 0, len(batch.Records))
	for _, rec := range batch.Records {
		if rec.IsCommandRecord() {
			continue
		}
		ev, ok := recordToSequencedEvent(rec)
		if !ok {
			log.Warn().
				Uint64("seq_num", rec.SeqNum).
				Str("conv_id", convID.String()).
				Msg("s2 read range: dropping record with missing/unknown type header")
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// ----------------------------------------------------------------------
// CheckTail
// ----------------------------------------------------------------------

func (s *s2StoreImpl) CheckTail(ctx context.Context, convID uuid.UUID) (uint64, error) {
	resp, err := s.stream(convID).CheckTail(ctx)
	if err != nil {
		return 0, mapS2Err(err)
	}
	return resp.Tail.SeqNum, nil
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// eventToRecord marshals a model.Event into the on-wire S2 append record.
// The event type lives in a header so readers can dispatch without JSON
// decoding, matching the contract in event-model-plan.md §5.
func eventToRecord(ev model.Event) (s2.AppendRecord, error) {
	body, err := ev.MarshalBody()
	if err != nil {
		return s2.AppendRecord{}, err
	}
	return s2.AppendRecord{
		Headers: []s2.Header{s2.NewHeader(typeHeaderName, string(ev.Type))},
		Body:    body,
	}, nil
}

// recordToSequencedEvent decodes an S2 record into the read-path value.
// Returns ok=false when the `type` header is missing; caller logs + skips.
func recordToSequencedEvent(rec s2.SequencedRecord) (model.SequencedEvent, bool) {
	var etype model.EventType
	found := false
	for _, h := range rec.Headers {
		if string(h.Name) == typeHeaderName {
			etype = model.EventType(h.Value)
			found = true
			break
		}
	}
	if !found || etype == "" {
		return model.SequencedEvent{}, false
	}

	// S2 timestamps are milliseconds since Unix epoch (arrival mode).
	ts := time.UnixMilli(int64(rec.Timestamp)).UTC()

	// Defensive copy of the body so callers can hold it past the SDK buffer.
	body := append(json.RawMessage(nil), rec.Body...)

	return model.SequencedEvent{
		SeqNum:    rec.SeqNum,
		Timestamp: ts,
		Type:      etype,
		Data:      body,
	}, true
}

// mapS2Err translates SDK-specific errors to store sentinels where we care,
// and passes everything else through untouched.
func mapS2Err(err error) error {
	if err == nil {
		return nil
	}
	var rnse *s2.RangeNotSatisfiableError
	if errors.As(err, &rnse) {
		return fmt.Errorf("%w: %v", ErrRangeNotSatisfiable, err)
	}
	return err
}
