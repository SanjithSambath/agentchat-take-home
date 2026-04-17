package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAckCursor_Happy(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	// Send a message to advance head_seq.
	_ = sendMessage(t, h, a, convID, newV7(t), "hello")

	body, _ := json.Marshal(AckRequest{Seq: 1})
	req := httptest.NewRequest(http.MethodPost, "/conversations/"+convID.String()+"/ack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
}

func TestAckCursor_BeyondHead(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	body, _ := json.Marshal(AckRequest{Seq: 9999})
	req := httptest.NewRequest(http.MethodPost, "/conversations/"+convID.String()+"/ack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var env APIError
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != CodeAckBeyondHead {
		t.Fatalf("code = %q, want %q", env.Error.Code, CodeAckBeyondHead)
	}
}

func TestListUnread(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	b := registerAgent(t, meta)
	rA := routeHelper(t, h, a)
	convID := createConv(t, rA)

	// Invite B.
	body, _ := json.Marshal(InviteRequest{AgentID: b})
	req := httptest.NewRequest(http.MethodPost, "/conversations/"+convID.String()+"/invite", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rA.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body.String())
	}

	// A sends a message.
	_ = sendMessage(t, h, a, convID, newV7(t), "hi")

	// B calls unread.
	rB := routeHelper(t, h, b)
	req = httptest.NewRequest(http.MethodGet, "/agents/me/unread", nil)
	rec = httptest.NewRecorder()
	rB.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp UnreadResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Conversations) == 0 {
		t.Fatalf("expected at least one unread entry, got 0")
	}
	if resp.Conversations[0].EventDelta == 0 {
		t.Fatalf("event_delta should be > 0: %+v", resp.Conversations[0])
	}
}
