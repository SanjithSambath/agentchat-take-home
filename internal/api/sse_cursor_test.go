package api

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// sseCursorRouter wires SSE + send-message + ack on top of the fake meta,
// mirroring sseRouter but adding the ack endpoint so we can exercise its
// delivery_seq update.
func sseCursorRouter(t *testing.T, h *Handler, meta *fakeMeta) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			raw := req.Header.Get("X-Agent-ID")
			id, err := uuid.Parse(raw)
			if err != nil {
				http.Error(w, "bad agent", http.StatusBadRequest)
				return
			}
			ok, _ := meta.AgentExists(req.Context(), id)
			if !ok {
				http.Error(w, "unknown agent", http.StatusNotFound)
				return
			}
			next.ServeHTTP(w, req.WithContext(WithAgentID(req.Context(), id)))
		})
	})
	r.Post("/conversations", h.CreateConversation)
	r.Post("/conversations/{cid}/messages", h.SendMessage)
	r.Get("/conversations/{cid}/stream", h.SSEStream)
	r.Post("/conversations/{cid}/ack", h.AckCursor)
	return r
}

// TestSSE_DeliveryCursorDoesNotAdvanceOnFlush pins the phantom-advance fix.
//
// BEFORE this fix: the server advanced delivery_seq on each rc.Flush(),
// conflating "bytes handed to kernel send buffer" with "client received."
// Through a buffering proxy the client could see nothing, disconnect, and
// reconnect to a cursor already advanced past the unread events — events
// were silently skipped.
//
// AFTER the fix: delivery_seq only advances when the client explicitly
// confirms via Last-Event-ID on reconnect, or via POST /ack. This test
// verifies: open SSE, the server streams events, close the connection
// WITHOUT sending Last-Event-ID, and the stored delivery_seq is still
// zero — so the events would be replayed on reconnect.
func TestSSE_DeliveryCursorDoesNotAdvanceOnFlush(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)

	srv := httptest.NewServer(sseCursorRouter(t, h, meta))
	defer srv.Close()

	// Create conversation as A.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/conversations", nil)
	req.Header.Set("X-Agent-ID", a.String())
	createRes, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create conv: %v", err)
	}
	var conv CreateConversationResponse
	if err := decodeResp(createRes, &conv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cid := conv.ConversationID

	// Pre-condition: delivery_seq starts at zero.
	if got, _ := meta.GetDeliveryCursor(context.Background(), a, cid); got != 0 {
		t.Fatalf("pre-open delivery_seq = %d, want 0", got)
	}

	// Open SSE from seq 0 so the handler replays the agent_joined event
	// (seq 0) it emitted on conv create.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet,
		srv.URL+"/conversations/"+cid.String()+"/stream?from=0", nil)
	sseReq.Header.Set("X-Agent-ID", a.String())
	sseRes, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("sse open: %v", err)
	}
	defer sseRes.Body.Close()
	if sseRes.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d", sseRes.StatusCode)
	}

	// Send a message so events 1-N get written while the SSE is open.
	msgID := uuid.Must(uuid.NewV7())
	body := fmt.Sprintf(`{"message_id":"%s","content":"hello"}`, msgID)
	mreq, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/conversations/"+cid.String()+"/messages",
		strings.NewReader(body))
	mreq.Header.Set("X-Agent-ID", a.String())
	mreq.Header.Set("Content-Type", "application/json")
	mres, err := http.DefaultClient.Do(mreq)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	mres.Body.Close()

	// Read enough frames that the server has flushed events for agent_joined
	// + start + append + end = 4 events minimum.
	reader := bufio.NewReader(sseRes.Body)
	events := readSSEEvents(t, reader, 4, 2*time.Second)
	if len(events) < 4 {
		t.Fatalf("expected >=4 events on wire, got %d", len(events))
	}
	sseCancel()
	// Give the deferred FlushDeliveryCursorOne a moment to run.
	time.Sleep(150 * time.Millisecond)

	// The critical assertion: delivery_seq must NOT have been advanced by
	// the wire-flush path. We closed the stream without sending a
	// Last-Event-ID confirmation; the server has no evidence we received
	// anything.
	got, _ := meta.GetDeliveryCursor(context.Background(), a, cid)
	if got != 0 {
		t.Fatalf("delivery_seq = %d after flush-only stream, want 0 "+
			"(the server is advancing cursor without client confirmation — "+
			"this is the phantom-advance bug)", got)
	}
}

// TestSSE_DeliveryCursorAdvancesFromLastEventID verifies the confirmation
// path: when a client reconnects with Last-Event-ID: N, the server treats
// that as "I saw everything up through N" and advances the stored cursor.
// Subsequent connects without headers resume from there.
func TestSSE_DeliveryCursorAdvancesFromLastEventID(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)

	srv := httptest.NewServer(sseCursorRouter(t, h, meta))
	defer srv.Close()

	// Create conv + send one message so we have seqs to reference.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/conversations", nil)
	req.Header.Set("X-Agent-ID", a.String())
	createRes, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var conv CreateConversationResponse
	if err := decodeResp(createRes, &conv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cid := conv.ConversationID

	msgID := uuid.Must(uuid.NewV7())
	body := fmt.Sprintf(`{"message_id":"%s","content":"hi"}`, msgID)
	mreq, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/conversations/"+cid.String()+"/messages",
		strings.NewReader(body))
	mreq.Header.Set("X-Agent-ID", a.String())
	mreq.Header.Set("Content-Type", "application/json")
	mres, err := http.DefaultClient.Do(mreq)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	mres.Body.Close()

	// Open SSE with Last-Event-ID: 2. Server should (a) start streaming
	// from seq 3, and (b) advance stored delivery_seq to 2.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet,
		srv.URL+"/conversations/"+cid.String()+"/stream", nil)
	sseReq.Header.Set("X-Agent-ID", a.String())
	sseReq.Header.Set("Last-Event-ID", "2")
	sseRes, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	defer sseRes.Body.Close()
	if sseRes.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d", sseRes.StatusCode)
	}
	// Read at least the :ok handshake + one event to prove the handler
	// actually entered its loop (and therefore had the chance to call
	// UpdateDeliveryCursor from the Last-Event-ID parse branch).
	reader := bufio.NewReader(sseRes.Body)
	_ = readSSEEvents(t, reader, 1, 1*time.Second)
	sseCancel()
	time.Sleep(100 * time.Millisecond)

	// delivery_seq stores "next seq to deliver" = Last-Event-ID + 1.
	// Last-Event-ID: 2 means client saw through seq 2, so store 3.
	got, _ := meta.GetDeliveryCursor(context.Background(), a, cid)
	if got < 3 {
		t.Fatalf("delivery_seq = %d after Last-Event-ID: 2 handshake, want >=3 "+
			"(cursor is 'next-to-deliver', so Last-Event-ID: N → store N+1)", got)
	}
}

// TestAck_AdvancesDeliveryCursor verifies that POST /ack not only updates
// ack_seq (the existing behavior) but also advances delivery_seq. Acking
// seq N implies the client fully processed everything up to N, which by
// definition includes receipt — so delivery_seq can safely follow.
func TestAck_AdvancesDeliveryCursor(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)

	srv := httptest.NewServer(sseCursorRouter(t, h, meta))
	defer srv.Close()

	// Create conv + send a message to raise head_seq above the ack we'll
	// try to place.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/conversations", nil)
	req.Header.Set("X-Agent-ID", a.String())
	createRes, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var conv CreateConversationResponse
	if err := decodeResp(createRes, &conv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cid := conv.ConversationID

	msgID := uuid.Must(uuid.NewV7())
	mreq, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/conversations/"+cid.String()+"/messages",
		strings.NewReader(fmt.Sprintf(`{"message_id":"%s","content":"x"}`, msgID)))
	mreq.Header.Set("X-Agent-ID", a.String())
	mreq.Header.Set("Content-Type", "application/json")
	mres, err := http.DefaultClient.Do(mreq)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	mres.Body.Close()

	// Ack at seq 2.
	areq, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/conversations/"+cid.String()+"/ack",
		strings.NewReader(`{"seq":2}`))
	areq.Header.Set("X-Agent-ID", a.String())
	areq.Header.Set("Content-Type", "application/json")
	ares, err := http.DefaultClient.Do(areq)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	ares.Body.Close()
	if ares.StatusCode != http.StatusNoContent {
		t.Fatalf("ack status = %d", ares.StatusCode)
	}

	// delivery_seq stores "next seq to deliver" = acked seq + 1.
	got, _ := meta.GetDeliveryCursor(context.Background(), a, cid)
	if got < 3 {
		t.Fatalf("delivery_seq = %d after POST /ack seq=2, want >=3 "+
			"(ack must advance delivery_seq to next-to-deliver = ack+1 so SSE reconnects resume correctly)", got)
	}
}
