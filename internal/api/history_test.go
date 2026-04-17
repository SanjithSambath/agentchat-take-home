package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetHistory_FromAndBefore(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	// Write 3 complete messages.
	for i := 0; i < 3; i++ {
		rec := sendMessage(t, h, a, convID, newV7(t), fmt.Sprintf("msg-%d", i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("send %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}

	// DESC (before mode, default).
	req := httptest.NewRequest(http.MethodGet, "/conversations/"+convID.String()+"/messages?limit=10", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("desc status = %d: %s", rec.Code, rec.Body.String())
	}
	var desc HistoryResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &desc)
	if len(desc.Messages) != 3 {
		t.Fatalf("desc messages = %d, want 3", len(desc.Messages))
	}
	if desc.Messages[0].SeqStart < desc.Messages[1].SeqStart {
		t.Fatalf("desc order wrong: %v", desc.Messages)
	}

	// ASC (from mode).
	req = httptest.NewRequest(http.MethodGet, "/conversations/"+convID.String()+"/messages?from=1&limit=10", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var asc HistoryResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &asc)
	if len(asc.Messages) != 3 {
		t.Fatalf("asc messages = %d, want 3", len(asc.Messages))
	}
	if asc.Messages[0].SeqStart > asc.Messages[1].SeqStart {
		t.Fatalf("asc order wrong: %v", asc.Messages)
	}
}

func TestGetHistory_MutualExclusion(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	req := httptest.NewRequest(http.MethodGet,
		"/conversations/"+convID.String()+"/messages?from=1&before=9", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var env APIError
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != CodeMutuallyExclusiveCursors {
		t.Fatalf("code = %q, want %q", env.Error.Code, CodeMutuallyExclusiveCursors)
	}
}

func TestGetHistory_InvalidLimit(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	r := routeHelper(t, h, a)
	convID := createConv(t, r)

	req := httptest.NewRequest(http.MethodGet,
		"/conversations/"+convID.String()+"/messages?limit=999", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
