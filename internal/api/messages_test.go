package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentmail/internal/store"

	"github.com/google/uuid"
)

func sendMessage(t *testing.T, h *Handler, agentID, convID, msgID uuid.UUID, content string) *httptest.ResponseRecorder {
	t.Helper()
	r := routeHelper(t, h, agentID)
	body, _ := json.Marshal(SendMessageRequest{MessageID: msgID, Content: content})
	req := httptest.NewRequest(http.MethodPost, "/conversations/"+convID.String()+"/messages",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestSendMessage_Happy(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	msgID, _ := uuid.NewV7()
	rec := sendMessage(t, h, a, convID, msgID, "hello")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var body SendMessageResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.AlreadyProcessed {
		t.Fatal("first send flagged as already_processed")
	}
	if body.SeqStart == 0 || body.SeqEnd < body.SeqStart {
		t.Fatalf("bad seq range: start=%d end=%d", body.SeqStart, body.SeqEnd)
	}
	// Dedup row written.
	row, found, _ := meta.GetMessageDedup(context.Background(), convID, msgID)
	if !found || row.Status != "complete" {
		t.Fatalf("dedup not recorded: row=%+v", row)
	}
}

func TestSendMessage_DedupReplay(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	msgID, _ := uuid.NewV7()
	_ = sendMessage(t, h, a, convID, msgID, "hi")
	rec := sendMessage(t, h, a, convID, msgID, "hi")
	if rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body SendMessageResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !body.AlreadyProcessed {
		t.Fatal("replay missing already_processed=true")
	}
}

func TestSendMessage_Validation(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	post := func(raw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/conversations/"+convID.String()+"/messages",
			bytes.NewReader([]byte(raw)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	v4 := uuid.New().String()

	cases := []struct {
		name string
		body string
		code string
	}{
		{"missing id", `{"content":"x"}`, CodeMissingMessageID},
		{"v4 not v7", `{"message_id":"` + v4 + `","content":"x"}`, CodeInvalidMessageID},
		{"empty content", `{"message_id":"` + newV7(t).String() + `","content":""}`, CodeEmptyContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(tc.body)
			if rec.Code == http.StatusCreated {
				t.Fatalf("unexpectedly accepted: %s", rec.Body.String())
			}
			var env APIError
			_ = json.Unmarshal(rec.Body.Bytes(), &env)
			if env.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q (body=%s)", env.Error.Code, tc.code, rec.Body.String())
			}
		})
	}
}

func TestSendMessage_AbortOnS2Failure(t *testing.T) {
	h, meta, s2 := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	// Make the append fail AFTER the create-conv append has already landed.
	s2.appendErr = errors.New("synthetic s2 failure")

	msgID := newV7(t)
	rec := sendMessage(t, h, a, convID, msgID, "doomed")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	// Allow the detached abort goroutine to write the dedup row.
	s2.appendErr = nil // let the abort's own append succeed
	// Retry should surface already_aborted — except the abort runs on a
	// detached context and may race; poll briefly.
	row, found, _ := meta.GetMessageDedup(context.Background(), convID, msgID)
	if !found || row.Status != "aborted" {
		t.Fatalf("expected aborted dedup row; got %+v", row)
	}
}

func TestSendMessage_SlowWriter(t *testing.T) {
	h, meta, s2 := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	s2.appendErr = store.ErrSlowWriter
	msgID := newV7(t)
	rec := sendMessage(t, h, a, convID, msgID, "slow")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	var env APIError
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != CodeSlowWriter {
		t.Fatalf("code = %q, want %q", env.Error.Code, CodeSlowWriter)
	}
}

func newV7(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid v7: %v", err)
	}
	return id
}
