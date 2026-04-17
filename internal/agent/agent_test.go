package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/uuid"

	"agentmail/internal/model"
	"agentmail/internal/store"
)

// ---------------------------------------------------------------------------
// In-memory S2Store (stream per conversation, blocking tail reader)
// ---------------------------------------------------------------------------

type fakeS2 struct {
	mu      sync.Mutex
	streams map[uuid.UUID]*fakeStream
}

func newFakeS2() *fakeS2 {
	return &fakeS2{streams: make(map[uuid.UUID]*fakeStream)}
}

type fakeStream struct {
	mu      sync.Mutex
	events  []model.SequencedEvent
	nextSeq uint64
	cond    *sync.Cond
}

func (fs *fakeS2) stream(convID uuid.UUID) *fakeStream {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	s, ok := fs.streams[convID]
	if !ok {
		s = &fakeStream{}
		s.cond = sync.NewCond(&s.mu)
		fs.streams[convID] = s
	}
	return s
}

func (fs *fakeS2) AppendEvents(ctx context.Context, convID uuid.UUID, events []model.Event) (store.AppendResult, error) {
	s := fs.stream(convID)
	s.mu.Lock()
	defer s.mu.Unlock()
	start := s.nextSeq
	for _, e := range events {
		body, err := e.MarshalBody()
		if err != nil {
			return store.AppendResult{}, err
		}
		s.nextSeq++
		s.events = append(s.events, model.SequencedEvent{
			SeqNum:    s.nextSeq,
			Timestamp: time.Now().UTC(),
			Type:      e.Type,
			Data:      body,
		})
	}
	s.cond.Broadcast()
	return store.AppendResult{StartSeq: start + 1, EndSeq: s.nextSeq + 1}, nil
}

type fakeAppendSession struct {
	s2     *fakeS2
	convID uuid.UUID
	closed bool
}

func (a *fakeAppendSession) Submit(ctx context.Context, event model.Event) error {
	if a.closed {
		return errors.New("submit after close")
	}
	_, err := a.s2.AppendEvents(ctx, a.convID, []model.Event{event})
	return err
}

func (a *fakeAppendSession) Close() error {
	a.closed = true
	return nil
}

func (fs *fakeS2) OpenAppendSession(ctx context.Context, convID uuid.UUID) (store.AppendSession, error) {
	return &fakeAppendSession{s2: fs, convID: convID}, nil
}

type fakeReadSession struct {
	ctx     context.Context
	stream  *fakeStream
	fromSeq uint64
	cur     model.SequencedEvent
	err     error
	closed  bool
}

func (rs *fakeReadSession) Next() bool {
	rs.stream.mu.Lock()
	defer rs.stream.mu.Unlock()
	for {
		if rs.closed {
			return false
		}
		if err := rs.ctx.Err(); err != nil {
			rs.err = err
			return false
		}
		// Find the first event with SeqNum >= fromSeq.
		for i := range rs.stream.events {
			if rs.stream.events[i].SeqNum >= rs.fromSeq {
				rs.cur = rs.stream.events[i]
				rs.fromSeq = rs.cur.SeqNum + 1
				return true
			}
		}
		// Wait for new events or ctx cancel. Use a goroutine to unblock
		// cond on ctx.Done() since cond.Wait() doesn't take a context.
		done := make(chan struct{})
		go func() {
			select {
			case <-rs.ctx.Done():
				rs.stream.mu.Lock()
				rs.stream.cond.Broadcast()
				rs.stream.mu.Unlock()
			case <-done:
			}
		}()
		rs.stream.cond.Wait()
		close(done)
	}
}

func (rs *fakeReadSession) Event() model.SequencedEvent { return rs.cur }
func (rs *fakeReadSession) Err() error                  { return rs.err }
func (rs *fakeReadSession) Close() error {
	rs.stream.mu.Lock()
	rs.closed = true
	rs.stream.cond.Broadcast()
	rs.stream.mu.Unlock()
	return nil
}

func (fs *fakeS2) OpenReadSession(ctx context.Context, convID uuid.UUID, fromSeq uint64) (store.ReadSession, error) {
	s := fs.stream(convID)
	if fromSeq == 0 {
		fromSeq = 1
	}
	return &fakeReadSession{ctx: ctx, stream: s, fromSeq: fromSeq}, nil
}

func (fs *fakeS2) ReadRange(ctx context.Context, convID uuid.UUID, fromSeq uint64, limit int) ([]model.SequencedEvent, error) {
	s := fs.stream(convID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if fromSeq == 0 {
		fromSeq = 1
	}
	var out []model.SequencedEvent
	for i := range s.events {
		if s.events[i].SeqNum >= fromSeq {
			out = append(out, s.events[i])
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (fs *fakeS2) CheckTail(ctx context.Context, convID uuid.UUID) (uint64, error) {
	s := fs.stream(convID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextSeq, nil
}

// ---------------------------------------------------------------------------
// In-memory MetadataStore — only the methods the agent uses (others panic)
// ---------------------------------------------------------------------------

type fakeMeta struct {
	mu           sync.Mutex
	agents       map[uuid.UUID]bool
	convs        map[uuid.UUID][]uuid.UUID // convID -> members
	cursors      map[string]uint64         // "<agent>/<conv>" -> seq
	inProgress   map[uuid.UUID]store.InProgressRow
	dedup        map[string]store.DedupRow // "<conv>/<msg>"
	completeLog  []uuid.UUID               // msgIDs inserted as complete (observability)
	abortedLog   []uuid.UUID
	inProgressWg sync.WaitGroup
}

func newFakeMeta() *fakeMeta {
	return &fakeMeta{
		agents:     make(map[uuid.UUID]bool),
		convs:      make(map[uuid.UUID][]uuid.UUID),
		cursors:    make(map[string]uint64),
		inProgress: make(map[uuid.UUID]store.InProgressRow),
		dedup:      make(map[string]store.DedupRow),
	}
}

func (m *fakeMeta) CreateAgent(ctx context.Context) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	m.mu.Lock()
	m.agents[id] = true
	m.mu.Unlock()
	return id, nil
}

func (m *fakeMeta) AgentExists(ctx context.Context, id uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.agents[id], nil
}

func (m *fakeMeta) CreateConversation(ctx context.Context, id uuid.UUID, s2StreamName string) error {
	m.mu.Lock()
	if _, ok := m.convs[id]; !ok {
		m.convs[id] = nil
	}
	m.mu.Unlock()
	return nil
}

func (m *fakeMeta) GetConversation(ctx context.Context, id uuid.UUID) (*store.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.convs[id]; !ok {
		return nil, store.ErrConversationNotFound
	}
	return &store.Conversation{ID: id, S2StreamName: fmt.Sprintf("conversations/%s", id)}, nil
}

func (m *fakeMeta) ConversationExists(ctx context.Context, id uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.convs[id]
	return ok, nil
}

func (m *fakeMeta) GetConversationHeadSeq(ctx context.Context, id uuid.UUID) (uint64, error) {
	return 0, nil
}

func (m *fakeMeta) UpdateConversationHeadSeq(ctx context.Context, id uuid.UUID, headSeq uint64) error {
	return nil
}

func (m *fakeMeta) AddMember(ctx context.Context, convID, agentID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	members := m.convs[convID]
	for _, a := range members {
		if a == agentID {
			return false, nil
		}
	}
	m.convs[convID] = append(members, agentID)
	return true, nil
}

func (m *fakeMeta) RemoveMember(ctx context.Context, convID, agentID uuid.UUID) error {
	return nil
}

func (m *fakeMeta) IsMember(ctx context.Context, agentID, convID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.convs[convID] {
		if a == agentID {
			return true, nil
		}
	}
	return false, nil
}

func (m *fakeMeta) ListMembers(ctx context.Context, convID uuid.UUID) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]uuid.UUID(nil), m.convs[convID]...), nil
}

func (m *fakeMeta) ListConversationsForAgent(ctx context.Context, agentID uuid.UUID) ([]store.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Conversation
	for cID, members := range m.convs {
		for _, a := range members {
			if a == agentID {
				out = append(out, store.Conversation{ID: cID, S2StreamName: fmt.Sprintf("conversations/%s", cID)})
				break
			}
		}
	}
	return out, nil
}

func (m *fakeMeta) LockMembersForUpdate(ctx context.Context, convID uuid.UUID) ([]uuid.UUID, error) {
	return m.ListMembers(ctx, convID)
}

func (m *fakeMeta) ListAllConversations(ctx context.Context) ([]store.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Conversation, 0, len(m.convs))
	for cID := range m.convs {
		out = append(out, store.Conversation{ID: cID, S2StreamName: fmt.Sprintf("conversations/%s", cID)})
	}
	return out, nil
}

func cursorKey(a, c uuid.UUID) string { return a.String() + "/" + c.String() }

func (m *fakeMeta) GetDeliveryCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursors[cursorKey(agentID, convID)], nil
}

func (m *fakeMeta) UpdateDeliveryCursor(agentID, convID uuid.UUID, seqNum uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := m.cursors[cursorKey(agentID, convID)]; seqNum > cur {
		m.cursors[cursorKey(agentID, convID)] = seqNum
	}
}

func (m *fakeMeta) FlushDeliveryCursorOne(ctx context.Context, agentID, convID uuid.UUID) error {
	return nil
}

func (m *fakeMeta) FlushDeliveryCursorAll(ctx context.Context) error { return nil }

func (m *fakeMeta) StartDeliveryCursorFlusher(ctx context.Context) {}

func (m *fakeMeta) Ack(ctx context.Context, agentID, convID uuid.UUID, seqNum uint64) error {
	return nil
}

func (m *fakeMeta) GetAckCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error) {
	return 0, nil
}

func (m *fakeMeta) ListUnreadForAgent(ctx context.Context, agentID uuid.UUID, limit int32) ([]store.UnreadEntry, error) {
	return nil, nil
}

func (m *fakeMeta) ClaimInProgressMessage(ctx context.Context, messageID, convID, agentID uuid.UUID, s2StreamName string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.inProgress[messageID]; ok {
		return false, nil
	}
	m.inProgress[messageID] = store.InProgressRow{
		MessageID:      messageID,
		ConversationID: convID,
		AgentID:        agentID,
		S2StreamName:   s2StreamName,
		StartedAt:      time.Now(),
	}
	return true, nil
}

func (m *fakeMeta) DeleteInProgressMessage(ctx context.Context, messageID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inProgress, messageID)
	return nil
}

func (m *fakeMeta) ListInProgressMessages(ctx context.Context) ([]store.InProgressRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.InProgressRow
	for _, r := range m.inProgress {
		out = append(out, r)
	}
	return out, nil
}

func dedupKey(c, msg uuid.UUID) string { return c.String() + "/" + msg.String() }

func (m *fakeMeta) GetMessageDedup(ctx context.Context, convID, messageID uuid.UUID) (*store.DedupRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.dedup[dedupKey(convID, messageID)]
	if !ok {
		return nil, false, nil
	}
	return &row, true, nil
}

func (m *fakeMeta) InsertMessageDedupComplete(ctx context.Context, convID, messageID uuid.UUID, startSeq, endSeq uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dedup[dedupKey(convID, messageID)] = store.DedupRow{
		Status:   model.StatusComplete,
		StartSeq: &startSeq,
		EndSeq:   &endSeq,
	}
	m.completeLog = append(m.completeLog, messageID)
	return nil
}

func (m *fakeMeta) InsertMessageDedupAborted(ctx context.Context, convID, messageID uuid.UUID, startSeq *uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dedup[dedupKey(convID, messageID)] = store.DedupRow{
		Status:   model.StatusAborted,
		StartSeq: startSeq,
	}
	m.abortedLog = append(m.abortedLog, messageID)
	return nil
}

func (m *fakeMeta) Ping(ctx context.Context) error { return nil }

func (m *fakeMeta) Close() error { return nil }

// ---------------------------------------------------------------------------
// Fake Claude streamer (for token-level control)
// ---------------------------------------------------------------------------

type fakeClaudeStream struct {
	deltas   []string
	pos      int
	err      error
	onNext   func() // hook invoked before each Next returns true (to inject latency)
	closed   bool
	errAfter int // if > 0, return err after this many deltas
}

func (s *fakeClaudeStream) Next() bool {
	if s.closed {
		return false
	}
	if s.errAfter > 0 && s.pos >= s.errAfter {
		s.err = errors.New("stream died")
		return false
	}
	if s.pos >= len(s.deltas) {
		return false
	}
	if s.onNext != nil {
		s.onNext()
	}
	s.pos++
	return true
}

func (s *fakeClaudeStream) Current() anthropic.MessageStreamEventUnion {
	text := s.deltas[s.pos-1]
	// Construct a MessageStreamEventUnion that AsAny() decodes to
	// ContentBlockDeltaEvent with a TextDelta. Easiest path: marshal the
	// minimal JSON and let the SDK's UnmarshalJSON do the rest.
	raw := fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, quote(text))
	var ev anthropic.MessageStreamEventUnion
	_ = json.Unmarshal([]byte(raw), &ev)
	return ev
}

func (s *fakeClaudeStream) Err() error { return s.err }
func (s *fakeClaudeStream) Close() error {
	s.closed = true
	return nil
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

type fakeStreamer struct {
	mu      sync.Mutex
	stream  *fakeClaudeStream
	calls   int
	builder func(attempt int) *fakeClaudeStream
}

func (f *fakeStreamer) Stream(ctx context.Context, msgs []anthropic.MessageParam) claudeStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.builder != nil {
		return f.builder(f.calls - 1)
	}
	// Fresh copy so repeated tests that call Stream twice get independent state.
	s := *f.stream
	return &s
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestAgent(t *testing.T, s2 *fakeS2, meta *fakeMeta, streamer claudeStreamer) *Agent {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	meta.agents[id] = true
	a, err := newAgentWithStreamer(id, s2, meta, streamer)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

func mustStart(t *testing.T, a *Agent) {
	t.Helper()
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("agent start: %v", err)
	}
}

func shutdown(a *Agent) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.Shutdown(ctx)
}

func otherMessage(t *testing.T, s2 *fakeS2, convID, sender uuid.UUID, content string) {
	t.Helper()
	msgID := uuid.Must(uuid.NewV7())
	_, err := s2.AppendEvents(context.Background(), convID, model.NewCompleteMessageEvents(msgID, sender, content))
	if err != nil {
		t.Fatalf("append message: %v", err)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, label string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", label)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRegisterIdentity_AgentRowPresent(t *testing.T) {
	s2 := newFakeS2()
	meta := newFakeMeta()
	a := newTestAgent(t, s2, meta, &fakeStreamer{stream: &fakeClaudeStream{}})
	mustStart(t, a)
	defer shutdown(a)
	if !a.Available() {
		t.Fatal("agent should be Available after Start")
	}
}

func TestStartTwice(t *testing.T) {
	s2 := newFakeS2()
	meta := newFakeMeta()
	a := newTestAgent(t, s2, meta, &fakeStreamer{stream: &fakeClaudeStream{}})
	mustStart(t, a)
	defer shutdown(a)
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("second Start must error")
	}
}

func TestStartupReconciliation(t *testing.T) {
	s2 := newFakeS2()
	meta := newFakeMeta()
	a := newTestAgent(t, s2, meta, &fakeStreamer{stream: &fakeClaudeStream{}})

	// Pre-seed three conversations the agent is a member of.
	convs := []uuid.UUID{uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())}
	for _, c := range convs {
		meta.convs[c] = []uuid.UUID{a.id}
	}

	mustStart(t, a)
	defer shutdown(a)

	waitUntil(t, 500*time.Millisecond, "three listeners", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.listeners) == 3
	})
}

func TestInviteDelivers(t *testing.T) {
	s2 := newFakeS2()
	meta := newFakeMeta()
	a := newTestAgent(t, s2, meta, &fakeStreamer{stream: &fakeClaudeStream{}})
	mustStart(t, a)
	defer shutdown(a)

	convID := uuid.Must(uuid.NewV7())
	meta.convs[convID] = []uuid.UUID{a.id}
	a.NotifyInvite(convID)

	waitUntil(t, 500*time.Millisecond, "listener started for invite", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.listeners[convID]
		return ok
	})
}

func TestSelfEchoSkipped(t *testing.T) {
	s2 := newFakeS2()
	meta := newFakeMeta()
	streamer := &fakeStreamer{stream: &fakeClaudeStream{deltas: []string{"hi"}}}
	a := newTestAgent(t, s2, meta, streamer)
	mustStart(t, a)
	defer shutdown(a)

	convID := uuid.Must(uuid.NewV7())
	meta.convs[convID] = []uuid.UUID{a.id}
	a.NotifyInvite(convID)
	waitUntil(t, 500*time.Millisecond, "listener up", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.listeners[convID]
		return ok
	})

	// Append a message authored by the agent itself.
	otherMessage(t, s2, convID, a.id, "self message")

	// Give the listener time to process; Claude must NOT be called.
	time.Sleep(100 * time.Millisecond)
	streamer.mu.Lock()
	calls := streamer.calls
	streamer.mu.Unlock()
	if calls != 0 {
		t.Fatalf("Claude should not be invoked for self-echo, got calls=%d", calls)
	}
}

func TestClaudeStreamingToS2(t *testing.T) {
	s2 := newFakeS2()
	meta := newFakeMeta()
	streamer := &fakeStreamer{stream: &fakeClaudeStream{deltas: []string{"Hello", " ", "world"}}}
	a := newTestAgent(t, s2, meta, streamer)
	mustStart(t, a)
	defer shutdown(a)

	convID := uuid.Must(uuid.NewV7())
	otherID := uuid.Must(uuid.NewV7())
	meta.convs[convID] = []uuid.UUID{a.id, otherID}
	a.NotifyInvite(convID)
	waitUntil(t, 500*time.Millisecond, "listener up", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.listeners[convID]
		return ok
	})

	otherMessage(t, s2, convID, otherID, "hey")

	// Wait until we see a message_end authored by the agent itself.
	waitUntil(t, 2*time.Second, "agent message_end", func() bool {
		st := s2.stream(convID)
		st.mu.Lock()
		defer st.mu.Unlock()
		for _, e := range st.events {
			if e.Type == model.EventTypeMessageStart {
				p, _ := model.ParsePayload[model.MessageStartPayload](e)
				if p.SenderID == a.id {
					// Found start — check we also saw end.
					for _, e2 := range st.events {
						if e2.Type == model.EventTypeMessageEnd {
							ep, _ := model.ParsePayload[model.MessageEndPayload](e2)
							if ep.MessageID == p.MessageID {
								return true
							}
						}
					}
				}
			}
		}
		return false
	})

	// Verify the assembled agent message equals "Hello world".
	st := s2.stream(convID)
	st.mu.Lock()
	defer st.mu.Unlock()
	msgs := model.AssembleMessages(st.events)
	found := false
	for _, m := range msgs {
		if m.SenderID == a.id && m.Status == model.StatusComplete {
			if m.Content != "Hello world" {
				t.Fatalf("assembled content = %q, want %q", m.Content, "Hello world")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no complete agent-authored message found on stream")
	}

	// Dedup row should be complete.
	meta.mu.Lock()
	defer meta.mu.Unlock()
	if len(meta.completeLog) != 1 {
		t.Fatalf("expected 1 complete dedup row, got %d", len(meta.completeLog))
	}
	if len(meta.inProgress) != 0 {
		t.Fatalf("expected 0 in-progress rows, got %d", len(meta.inProgress))
	}
}

func TestAbortOnStreamError(t *testing.T) {
	s2 := newFakeS2()
	meta := newFakeMeta()
	// Emit one delta, then error. anyText will be true, so retries won't
	// kick in; we should see an abort on-stream.
	streamer := &fakeStreamer{builder: func(attempt int) *fakeClaudeStream {
		return &fakeClaudeStream{deltas: []string{"partial"}, errAfter: 1}
	}}
	a := newTestAgent(t, s2, meta, streamer)
	mustStart(t, a)
	defer shutdown(a)

	convID := uuid.Must(uuid.NewV7())
	otherID := uuid.Must(uuid.NewV7())
	meta.convs[convID] = []uuid.UUID{a.id, otherID}
	a.NotifyInvite(convID)
	waitUntil(t, 500*time.Millisecond, "listener up", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.listeners[convID]
		return ok
	})
	otherMessage(t, s2, convID, otherID, "trigger")

	waitUntil(t, 2*time.Second, "abort recorded", func() bool {
		meta.mu.Lock()
		defer meta.mu.Unlock()
		return len(meta.abortedLog) == 1 && len(meta.inProgress) == 0
	})
}

func TestBuildClaudeMessages_MergeSameRole(t *testing.T) {
	self := uuid.Must(uuid.NewV7())
	a := uuid.Must(uuid.NewV7())
	b := uuid.Must(uuid.NewV7())
	history := []convMessage{
		{Sender: a, Content: "first from A"},
		{Sender: b, Content: "first from B"},
		{Sender: self, Content: "assistant reply"},
		{Sender: a, Content: "follow-up A"},
	}
	msgs := buildClaudeMessages(history, self)
	if len(msgs) != 3 {
		t.Fatalf("want 3 merged turns, got %d", len(msgs))
	}
	if msgs[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("first turn role = %q, want user", msgs[0].Role)
	}
	if msgs[1].Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("second turn role = %q, want assistant", msgs[1].Role)
	}
	if msgs[2].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("third turn role = %q, want user", msgs[2].Role)
	}
}

func TestSlidingWindow(t *testing.T) {
	state := &convState{}
	self := uuid.Must(uuid.NewV7())
	other := uuid.Must(uuid.NewV7())
	for i := 0; i < maxHistory*2; i++ {
		sender := other
		if i%2 == 0 {
			sender = self
		}
		appendMessage(state, convMessage{Sender: sender, Content: fmt.Sprintf("%d", i)})
	}
	if len(state.history) != maxHistory {
		t.Fatalf("history = %d, want %d", len(state.history), maxHistory)
	}
	last := state.history[maxHistory-1].Content
	if last != fmt.Sprintf("%d", maxHistory*2-1) {
		t.Fatalf("last content = %q, want %q", last, fmt.Sprintf("%d", maxHistory*2-1))
	}
}

func TestLazyHistorySeeding(t *testing.T) {
	s2 := newFakeS2()
	meta := newFakeMeta()
	streamer := &fakeStreamer{stream: &fakeClaudeStream{deltas: []string{"ok"}}}
	a := newTestAgent(t, s2, meta, streamer)

	convID := uuid.Must(uuid.NewV7())
	otherID := uuid.Must(uuid.NewV7())
	meta.convs[convID] = []uuid.UUID{a.id, otherID}

	// Pre-seed 5 complete messages from other before the agent starts.
	for i := 0; i < 5; i++ {
		otherMessage(t, s2, convID, otherID, fmt.Sprintf("prior %d", i))
	}

	mustStart(t, a)
	defer shutdown(a)
	a.NotifyInvite(convID)

	// Issue a new message from other → should trigger seed + respond.
	otherMessage(t, s2, convID, otherID, "new one")

	// Wait for at least one response to complete. The agent may respond
	// once (if a later event arrived while the first response was
	// in-flight and got skipped) or more, depending on scheduling; either
	// is correct. We just verify the agent responded at all.
	waitUntil(t, 2*time.Second, "agent responded", func() bool {
		meta.mu.Lock()
		defer meta.mu.Unlock()
		return len(meta.completeLog) >= 1
	})

	// Assert the seed-on-first-message_end invariant: state is marked
	// seeded, and history contains the other-agent messages.
	state := a.getOrCreateConvState(convID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.seeded {
		t.Fatal("seeded should be true after first message_end")
	}
	// history contains (at minimum) some of the 5 prior + the "new one" +
	// the agent's own reply. The exact count depends on scheduling; we
	// assert it's non-trivial.
	if len(state.history) < 2 {
		t.Fatalf("history = %d entries, want >= 2", len(state.history))
	}
}

func TestLeaveAbortsInFlight(t *testing.T) {
	s2 := newFakeS2()
	meta := newFakeMeta()
	// A stream that emits one delta then blocks indefinitely on Next —
	// we'll cancel the listener mid-stream to trigger abort.
	block := make(chan struct{})
	var once sync.Once
	streamer := &fakeStreamer{builder: func(attempt int) *fakeClaudeStream {
		return &fakeClaudeStream{
			deltas: []string{"hello"},
			onNext: func() {
				// After the first delta has been pumped, release the
				// test so it can trigger NotifyLeave.
				once.Do(func() { close(block) })
			},
			errAfter: 999, // let it produce deltas forever
		}
	}}
	// The streamer above emits exactly 1 delta then stops (errAfter hit).
	// That's sufficient — we want anyText=true so abort path kicks in on
	// ctx cancel.
	_ = streamer

	// Simpler: use a stream that emits 1 delta and then errors, so the
	// retry gate is disabled (anyText=true), and the respond path returns
	// via abortAndRecord with reason claude_error. But for leave abort, we
	// want listener ctx cancel — use a blocking stream instead.
	streamer = &fakeStreamer{builder: func(attempt int) *fakeClaudeStream {
		return &fakeClaudeStream{
			deltas: []string{"h", "e", "l", "l", "o"},
			onNext: func() {
				// Slow the stream so leave arrives mid-pump.
				time.Sleep(30 * time.Millisecond)
				once.Do(func() { close(block) })
			},
		}
	}}

	a := newTestAgent(t, s2, meta, streamer)
	mustStart(t, a)
	defer shutdown(a)

	convID := uuid.Must(uuid.NewV7())
	otherID := uuid.Must(uuid.NewV7())
	meta.convs[convID] = []uuid.UUID{a.id, otherID}
	a.NotifyInvite(convID)
	waitUntil(t, 500*time.Millisecond, "listener up", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.listeners[convID]
		return ok
	})

	otherMessage(t, s2, convID, otherID, "trigger")

	// Wait until first delta has pumped, then trigger leave.
	select {
	case <-block:
	case <-time.After(time.Second):
		t.Fatal("stream never pumped")
	}

	done := a.NotifyLeave(convID)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("leave drain timed out")
	}

	// After drain, listener map should no longer contain the convID.
	a.mu.Lock()
	_, stillThere := a.listeners[convID]
	a.mu.Unlock()
	if stillThere {
		t.Fatal("listener should be removed after drain")
	}

	// Either complete or aborted must have been recorded; in-progress must
	// be empty.
	meta.mu.Lock()
	defer meta.mu.Unlock()
	if len(meta.inProgress) != 0 {
		t.Fatalf("inProgress not drained: %d", len(meta.inProgress))
	}
	if len(meta.completeLog)+len(meta.abortedLog) == 0 {
		t.Fatal("expected terminal dedup (complete or aborted) record")
	}
}

// ---------------------------------------------------------------------------
// Live smoke test — skipped without ANTHROPIC_API_KEY.
// ---------------------------------------------------------------------------

func TestLiveClaudeStreaming(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live Claude smoke test")
	}

	s2 := newFakeS2()
	meta := newFakeMeta()
	client := anthropic.NewClient()
	streamer := &anthropicStreamer{client: &client}

	id := uuid.Must(uuid.NewV7())
	meta.agents[id] = true
	a, err := newAgentWithStreamer(id, s2, meta, streamer)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	mustStart(t, a)
	defer shutdown(a)

	convID := uuid.Must(uuid.NewV7())
	otherID := uuid.Must(uuid.NewV7())
	meta.convs[convID] = []uuid.UUID{a.id, otherID}
	a.NotifyInvite(convID)
	waitUntil(t, 500*time.Millisecond, "listener up", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.listeners[convID]
		return ok
	})

	otherMessage(t, s2, convID, otherID, "Say hi in exactly one word.")

	waitUntil(t, 30*time.Second, "live response complete", func() bool {
		meta.mu.Lock()
		defer meta.mu.Unlock()
		return len(meta.completeLog) == 1
	})

	st := s2.stream(convID)
	st.mu.Lock()
	defer st.mu.Unlock()
	msgs := model.AssembleMessages(st.events)
	var agentMsg *model.HistoryMessage
	for i := range msgs {
		if msgs[i].SenderID == a.id && msgs[i].Status == model.StatusComplete {
			agentMsg = &msgs[i]
		}
	}
	if agentMsg == nil {
		t.Fatal("no complete agent message on stream")
	}
	if agentMsg.Content == "" {
		t.Fatal("agent message content is empty")
	}
	t.Logf("live Claude response: %q", agentMsg.Content)
}
