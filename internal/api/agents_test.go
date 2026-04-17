package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestCreateAgent(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/agents", nil)
	rec := httptest.NewRecorder()
	h.CreateAgent(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var body CreateAgentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AgentID == uuid.Nil {
		t.Fatal("agent_id is nil")
	}
}

func TestGetResidentAgent_Disabled(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/agents/resident", nil)
	rec := httptest.NewRecorder()
	h.GetResidentAgent(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	var env APIError
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != CodeResidentUnavailable {
		t.Fatalf("code = %q, want %q", env.Error.Code, CodeResidentUnavailable)
	}
}

// stubResident implements ResidentInfo with a fixed id.
type stubResident struct{ id uuid.UUID }

func (s stubResident) ID() uuid.UUID            { return s.id }
func (s stubResident) Available() bool          { return s.id != uuid.Nil }
func (s stubResident) NotifyInvite(uuid.UUID)   {}
func (s stubResident) NotifyLeave(uuid.UUID) <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func TestGetResidentAgent_Enabled(t *testing.T) {
	id, _ := uuid.NewV7()
	h := NewHandler(newFakeS2(), newFakeMeta(), stubResident{id: id}, nil)

	req := httptest.NewRequest(http.MethodGet, "/agents/resident", nil)
	rec := httptest.NewRecorder()
	h.GetResidentAgent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body GetResidentAgentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.AgentID != id {
		t.Fatalf("agent_id = %s, want %s", body.AgentID, id)
	}
}
