//go:build integration

package tests

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"agentmail/internal/api"
)

// TestResidentAgentRespondsToMessage bootstraps the real resident Claude
// agent, invites it into a conversation, sends a user message, and asserts
// that Claude streams back a response (message_start → append+ → end)
// within a generous deadline.
//
// Requires ANTHROPIC_API_KEY in addition to DATABASE_URL / S2_AUTH_TOKEN;
// skipped otherwise.
func TestResidentAgentRespondsToMessage(t *testing.T) {
	ts := startTestServer(t, true)
	defer ts.Close()
	if ts.Resident == nil {
		t.Fatal("test server did not bootstrap a resident agent")
	}
	residentID := ts.Resident.ID()

	// Sanity: GET /agents/resident returns the bootstrapped id.
	var resp api.GetResidentAgentResponse
	code, raw := ts.doJSON(t, http.MethodGet, "/agents/resident", uuid.Nil, nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("resident lookup: status=%d body=%s", code, raw)
	}
	if resp.AgentID != residentID {
		t.Fatalf("resident id mismatch: got %s, want %s", resp.AgentID, residentID)
	}

	// Human agent creates the conversation and invites the resident.
	user := ts.createAgent(t)
	conv := ts.createConversation(t, user)
	ts.invite(t, user, conv, residentID)

	// Observe from the human's perspective.
	sse := ts.openSSE(t, user, conv, "")
	defer sse.Close()
	// Drain the user's join + the resident's join.
	_ = sse.Collect(2, 5*time.Second)

	// Trigger a response.
	ts.sendMessage(t, user, conv, "Say the word HELLO in all caps and nothing else.")

	// Expect: the three frames from our own send first, then the resident's
	// start + >=1 append + end.
	deadline := time.Now().Add(45 * time.Second)
	var sawResidentStart, sawResidentEnd bool
	var residentMID string
	for time.Now().Before(deadline) {
		ev, ok := sse.Next(time.Until(deadline))
		if !ok {
			break
		}
		body := asMap(t, ev.Data)
		if ev.Type == "message_start" {
			sender, _ := body["sender_id"].(string)
			if sender == residentID.String() {
				sawResidentStart = true
				residentMID, _ = body["message_id"].(string)
			}
		}
		if ev.Type == "message_end" {
			mid, _ := body["message_id"].(string)
			if sawResidentStart && mid == residentMID {
				sawResidentEnd = true
				break
			}
		}
	}
	if !sawResidentStart {
		t.Fatalf("did not observe resident message_start within deadline")
	}
	if !sawResidentEnd {
		t.Fatalf("did not observe resident message_end within deadline")
	}
}
