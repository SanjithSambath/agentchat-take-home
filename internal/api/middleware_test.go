package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestAgentAuth(t *testing.T) {
	meta := newFakeMeta()
	knownID := registerAgent(t, meta)

	handler := AgentAuth(meta)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if AgentIDFromContext(r.Context()) != knownID {
			t.Fatalf("agent id not propagated via context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"missing header", "", http.StatusBadRequest},
		{"bad uuid", "not-a-uuid", http.StatusBadRequest},
		{"unknown agent", uuid.NewString(), http.StatusNotFound},
		{"happy path", knownID.String(), http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tc.header != "" {
				req.Header.Set("X-Agent-ID", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
