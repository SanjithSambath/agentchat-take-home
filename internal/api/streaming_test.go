package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func streamPost(t *testing.T, r http.Handler, convID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/conversations/"+convID.String()+"/messages/stream",
		bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/x-ndjson")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestStream_HappyPath(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	msgID := newV7(t)
	body := fmt.Sprintf(`{"message_id":"%s"}
{"content":"Hello "}
{"content":"world"}
`, msgID)

	rec := streamPost(t, r, convID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp StreamMessageResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.MessageID != msgID {
		t.Fatalf("message_id round-trip wrong: %s vs %s", resp.MessageID, msgID)
	}
	if resp.AlreadyProcessed {
		t.Fatal("first stream flagged as already_processed")
	}
}

func TestStream_MissingMessageID(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	rec := streamPost(t, r, convID, `{"nothing":"here"}
`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var env APIError
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != CodeMissingMessageID {
		t.Fatalf("code = %q, want %q", env.Error.Code, CodeMissingMessageID)
	}
}

func TestStream_DedupReplay(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	msgID := newV7(t)

	// First call completes.
	body := fmt.Sprintf(`{"message_id":"%s"}
{"content":"x"}
`, msgID)
	rec := streamPost(t, r, convID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("first stream: %d %s", rec.Code, rec.Body.String())
	}

	// Replay.
	rec = streamPost(t, r, convID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	var resp StreamMessageResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.AlreadyProcessed {
		t.Fatal("replay missing already_processed=true")
	}
}

func TestStream_WrongContentType(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	req := httptest.NewRequest(http.MethodPost,
		"/conversations/"+convID.String()+"/messages/stream",
		bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415: %s", rec.Code, rec.Body.String())
	}
}
