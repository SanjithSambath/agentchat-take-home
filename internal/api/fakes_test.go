package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"agentmail/internal/model"
	"agentmail/internal/store"

	"github.com/google/uuid"
)

// fakeMeta is a fully in-memory MetadataStore. All state lives under one
// mutex for simplicity; the API tests are not throughput-bound.
type fakeMeta struct {
	mu sync.Mutex

	agents      map[uuid.UUID]bool
	convs       map[uuid.UUID]*store.Conversation
	members     map[uuid.UUID]map[uuid.UUID]bool // convID -> set of agentIDs
	deliveries  map[cursorKey]uint64
	acks        map[cursorKey]uint64
	dedup       map[dedupKey]*store.DedupRow
	inProgress  map[uuid.UUID]store.InProgressRow
	headOffsets map[uuid.UUID]uint64

	// Optional hooks injected by tests.
	createAgentErr error
	isMemberErr    error
	addMemberHook  func(convID, agentID uuid.UUID) (bool, error)
	removeHook     func(convID, agentID uuid.UUID) error
}

type cursorKey struct{ Agent, Conv uuid.UUID }
type dedupKey struct{ Conv, Msg uuid.UUID }

func newFakeMeta() *fakeMeta {
	return &fakeMeta{
		agents:      make(map[uuid.UUID]bool),
		convs:       make(map[uuid.UUID]*store.Conversation),
		members:     make(map[uuid.UUID]map[uuid.UUID]bool),
		deliveries:  make(map[cursorKey]uint64),
		acks:        make(map[cursorKey]uint64),
		dedup:       make(map[dedupKey]*store.DedupRow),
		inProgress:  make(map[uuid.UUID]store.InProgressRow),
		headOffsets: make(map[uuid.UUID]uint64),
	}
}

func (f *fakeMeta) CreateAgent(ctx context.Context) (uuid.UUID, error) {
	if f.createAgentErr != nil {
		return uuid.Nil, f.createAgentErr
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	f.mu.Lock()
	f.agents[id] = true
	f.mu.Unlock()
	return id, nil
}

func (f *fakeMeta) AgentExists(ctx context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents[id], nil
}

func (f *fakeMeta) CreateConversation(ctx context.Context, id uuid.UUID, s2StreamName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.convs[id]; ok {
		return errors.New("fake: conv already exists")
	}
	f.convs[id] = &store.Conversation{
		ID: id, S2StreamName: s2StreamName, HeadSeq: 0, CreatedAt: time.Now().UTC(),
	}
	f.members[id] = make(map[uuid.UUID]bool)
	return nil
}

func (f *fakeMeta) GetConversation(ctx context.Context, id uuid.UUID) (*store.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.convs[id]
	if !ok {
		return nil, nil
	}
	copy := *c
	return &copy, nil
}

func (f *fakeMeta) ConversationExists(ctx context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.convs[id]
	return ok, nil
}

func (f *fakeMeta) GetConversationHeadSeq(ctx context.Context, id uuid.UUID) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.convs[id]
	if !ok {
		return 0, store.ErrConversationNotFound
	}
	return c.HeadSeq, nil
}

func (f *fakeMeta) UpdateConversationHeadSeq(ctx context.Context, id uuid.UUID, headSeq uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.convs[id]
	if !ok {
		return store.ErrConversationNotFound
	}
	if headSeq > c.HeadSeq {
		c.HeadSeq = headSeq
	}
	return nil
}

func (f *fakeMeta) AddMember(ctx context.Context, convID, agentID uuid.UUID) (bool, error) {
	if f.addMemberHook != nil {
		return f.addMemberHook(convID, agentID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[convID]
	if !ok {
		return false, store.ErrConversationNotFound
	}
	if m[agentID] {
		return false, nil
	}
	m[agentID] = true
	return true, nil
}

func (f *fakeMeta) RemoveMember(ctx context.Context, convID, agentID uuid.UUID) error {
	if f.removeHook != nil {
		return f.removeHook(convID, agentID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[convID]
	if !ok {
		return store.ErrConversationNotFound
	}
	if !m[agentID] {
		return store.ErrNotMember
	}
	if len(m) == 1 {
		return store.ErrLastMember
	}
	delete(m, agentID)
	return nil
}

func (f *fakeMeta) IsMember(ctx context.Context, agentID, convID uuid.UUID) (bool, error) {
	if f.isMemberErr != nil {
		return false, f.isMemberErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[convID]
	if !ok {
		return false, nil
	}
	return m[agentID], nil
}

func (f *fakeMeta) ListMembers(ctx context.Context, convID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[convID]
	if !ok {
		return nil, store.ErrConversationNotFound
	}
	out := make([]uuid.UUID, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	return out, nil
}

func (f *fakeMeta) ListConversationsForAgent(ctx context.Context, agentID uuid.UUID) ([]store.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Conversation
	for cid, members := range f.members {
		if members[agentID] {
			if c, ok := f.convs[cid]; ok {
				out = append(out, *c)
			}
		}
	}
	return out, nil
}

func (f *fakeMeta) LockMembersForUpdate(ctx context.Context, convID uuid.UUID) ([]uuid.UUID, error) {
	return f.ListMembers(ctx, convID)
}

func (f *fakeMeta) GetDeliveryCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deliveries[cursorKey{agentID, convID}], nil
}

func (f *fakeMeta) UpdateDeliveryCursor(agentID, convID uuid.UUID, seqNum uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := cursorKey{agentID, convID}
	if seqNum > f.deliveries[k] {
		f.deliveries[k] = seqNum
	}
}

func (f *fakeMeta) FlushDeliveryCursorOne(ctx context.Context, agentID, convID uuid.UUID) error {
	return nil
}

func (f *fakeMeta) FlushDeliveryCursorAll(ctx context.Context) error { return nil }

func (f *fakeMeta) StartDeliveryCursorFlusher(ctx context.Context) {}

func (f *fakeMeta) Ack(ctx context.Context, agentID, convID uuid.UUID, seqNum uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := cursorKey{agentID, convID}
	if seqNum > f.acks[k] {
		f.acks[k] = seqNum
	}
	return nil
}

func (f *fakeMeta) GetAckCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acks[cursorKey{agentID, convID}], nil
}

func (f *fakeMeta) ListUnreadForAgent(ctx context.Context, agentID uuid.UUID, limit int32) ([]store.UnreadEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.UnreadEntry
	for cid, members := range f.members {
		if !members[agentID] {
			continue
		}
		c, ok := f.convs[cid]
		if !ok {
			continue
		}
		ack := f.acks[cursorKey{agentID, cid}]
		if c.HeadSeq > ack {
			out = append(out, store.UnreadEntry{
				ConversationID: cid,
				HeadSeq:        c.HeadSeq,
				AckSeq:         ack,
				EventDelta:     c.HeadSeq - ack,
			})
		}
		if len(out) >= int(limit) {
			break
		}
	}
	return out, nil
}

func (f *fakeMeta) ClaimInProgressMessage(ctx context.Context, messageID, convID, agentID uuid.UUID, s2StreamName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.inProgress[messageID]; ok {
		return false, nil
	}
	f.inProgress[messageID] = store.InProgressRow{
		MessageID:      messageID,
		ConversationID: convID,
		AgentID:        agentID,
		S2StreamName:   s2StreamName,
		StartedAt:      time.Now().UTC(),
	}
	return true, nil
}

func (f *fakeMeta) DeleteInProgressMessage(ctx context.Context, messageID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.inProgress, messageID)
	return nil
}

func (f *fakeMeta) ListInProgressMessages(ctx context.Context) ([]store.InProgressRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.InProgressRow, 0, len(f.inProgress))
	for _, r := range f.inProgress {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeMeta) GetMessageDedup(ctx context.Context, convID, messageID uuid.UUID) (*store.DedupRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.dedup[dedupKey{convID, messageID}]
	if !ok {
		return nil, false, nil
	}
	copy := *row
	return &copy, true, nil
}

func (f *fakeMeta) InsertMessageDedupComplete(ctx context.Context, convID, messageID uuid.UUID, startSeq, endSeq uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, e := startSeq, endSeq
	f.dedup[dedupKey{convID, messageID}] = &store.DedupRow{Status: model.StatusComplete, StartSeq: &s, EndSeq: &e}
	return nil
}

func (f *fakeMeta) InsertMessageDedupAborted(ctx context.Context, convID, messageID uuid.UUID, startSeq *uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dedup[dedupKey{convID, messageID}] = &store.DedupRow{Status: model.StatusAborted, StartSeq: startSeq}
	return nil
}

func (f *fakeMeta) Ping(ctx context.Context) error { return nil }

func (f *fakeMeta) Close() error { return nil }

// --- fake S2 -----------------------------------------------------------

// fakeS2 is an in-memory stream keyed by conversation ID. Records receive
// monotonic 1-based seqs. Readers block on a per-conv condition variable
// until a new record is appended or ctx is cancelled.
type fakeS2 struct {
	mu      sync.Mutex
	streams map[uuid.UUID]*fakeStream

	// Hooks.
	appendErr   error
	submitDelay time.Duration
	submitErr   error
}

type fakeStream struct {
	mu     sync.Mutex
	cond   *sync.Cond
	events []model.SequencedEvent
}

func newFakeS2() *fakeS2 {
	return &fakeS2{streams: make(map[uuid.UUID]*fakeStream)}
}

func (f *fakeS2) stream(convID uuid.UUID) *fakeStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.streams[convID]
	if !ok {
		s = &fakeStream{}
		s.cond = sync.NewCond(&s.mu)
		f.streams[convID] = s
	}
	return s
}

func (f *fakeS2) AppendEvents(ctx context.Context, convID uuid.UUID, events []model.Event) (store.AppendResult, error) {
	if f.appendErr != nil {
		return store.AppendResult{}, f.appendErr
	}
	if err := ctx.Err(); err != nil {
		return store.AppendResult{}, err
	}
	s := f.stream(convID)
	s.mu.Lock()
	startSeq := uint64(len(s.events)) + 1
	for _, ev := range events {
		body, err := ev.MarshalBody()
		if err != nil {
			s.mu.Unlock()
			return store.AppendResult{}, err
		}
		se := model.SequencedEvent{
			SeqNum:    uint64(len(s.events)) + 1,
			Timestamp: time.Now().UTC(),
			Type:      ev.Type,
			Data:      body,
		}
		s.events = append(s.events, se)
	}
	endSeq := uint64(len(s.events)) + 1
	s.cond.Broadcast()
	s.mu.Unlock()
	return store.AppendResult{StartSeq: startSeq, EndSeq: endSeq}, nil
}

func (f *fakeS2) OpenAppendSession(ctx context.Context, convID uuid.UUID) (store.AppendSession, error) {
	if f.appendErr != nil {
		return nil, f.appendErr
	}
	return &fakeAppendSession{s2: f, convID: convID}, nil
}

type fakeAppendSession struct {
	s2     *fakeS2
	convID uuid.UUID
	closed bool
	mu     sync.Mutex
}

func (a *fakeAppendSession) Submit(ctx context.Context, ev model.Event) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return errors.New("fake: session closed")
	}
	a.mu.Unlock()
	if a.s2.submitDelay > 0 {
		select {
		case <-time.After(a.s2.submitDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if a.s2.submitErr != nil {
		return a.s2.submitErr
	}
	_, err := a.s2.AppendEvents(ctx, a.convID, []model.Event{ev})
	return err
}

func (a *fakeAppendSession) Close() error {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	return nil
}

func (f *fakeS2) OpenReadSession(ctx context.Context, convID uuid.UUID, fromSeq uint64) (store.ReadSession, error) {
	s := f.stream(convID)
	return &fakeReadSession{s: s, next: fromSeq, ctx: ctx}, nil
}

type fakeReadSession struct {
	s    *fakeStream
	next uint64
	cur  model.SequencedEvent
	ctx  context.Context
	err  error
}

func (r *fakeReadSession) Next() bool {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	// Start indexing at 1; our events are stored zero-indexed but seq is 1-based.
	for {
		if r.ctx.Err() != nil {
			r.err = r.ctx.Err()
			return false
		}
		if r.next < 1 {
			r.next = 1
		}
		idx := int(r.next) - 1
		if idx >= 0 && idx < len(r.s.events) {
			r.cur = r.s.events[idx]
			r.next++
			return true
		}
		// Wait for a broadcast or ctx cancel. sync.Cond doesn't honor ctx,
		// so we spawn a watchdog that broadcasts on cancel.
		done := make(chan struct{})
		go func() {
			select {
			case <-r.ctx.Done():
				r.s.mu.Lock()
				r.s.cond.Broadcast()
				r.s.mu.Unlock()
			case <-done:
			}
		}()
		r.s.cond.Wait()
		close(done)
	}
}

func (r *fakeReadSession) Event() model.SequencedEvent { return r.cur }
func (r *fakeReadSession) Err() error                  { return r.err }
func (r *fakeReadSession) Close() error {
	r.s.mu.Lock()
	r.s.cond.Broadcast()
	r.s.mu.Unlock()
	return nil
}

func (f *fakeS2) ReadRange(ctx context.Context, convID uuid.UUID, fromSeq uint64, limit int) ([]model.SequencedEvent, error) {
	s := f.stream(convID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if fromSeq < 1 {
		fromSeq = 1
	}
	start := int(fromSeq) - 1
	if start >= len(s.events) {
		return nil, nil
	}
	end := start + limit
	if end > len(s.events) {
		end = len(s.events)
	}
	out := make([]model.SequencedEvent, end-start)
	copy(out, s.events[start:end])
	return out, nil
}

func (f *fakeS2) CheckTail(ctx context.Context, convID uuid.UUID) (uint64, error) {
	s := f.stream(convID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return uint64(len(s.events)), nil
}

// --- test helpers ------------------------------------------------------

// registerAgent inserts a fresh agent into the fake meta and returns the id.
func registerAgent(t interface{ Fatalf(string, ...any) }, meta *fakeMeta) uuid.UUID {
	id, err := meta.CreateAgent(context.Background())
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return id
}

// newTestHandler wires a Handler with the in-memory fakes.
func newTestHandler() (*Handler, *fakeMeta, *fakeS2) {
	meta := newFakeMeta()
	s2 := newFakeS2()
	return NewHandler(s2, meta, DisabledResident{}, nil), meta, s2
}
