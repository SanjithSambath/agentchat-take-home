package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentmail/internal/api/client"
)

// TestClientRunAgentServed verifies GET /client/run_agent.py returns the
// embedded Python runtime with the expected shape: 200 OK, text/x-python
// Content-Type, X-Runtime-Version header, and byte-for-byte equality with
// the embedded file. Load-bearing — the Claude Code bootstrap curl's this
// endpoint on every first run.
func TestClientRunAgentServed(t *testing.T) {
	meta := newFakeMeta()
	s2 := newFakeS2()
	router := NewRouter(s2, meta, DisabledResident{}, nil)
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/client/run_agent.py", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /client/run_agent.py: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}

	ct := res.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/x-python") {
		t.Errorf("Content-Type = %q, want text/x-python prefix", ct)
	}
	if got := res.Header.Get("X-Runtime-Version"); got == "" {
		t.Error("missing X-Runtime-Version header")
	} else if got != client.Version {
		t.Errorf("X-Runtime-Version = %q, want %q", got, client.Version)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, client.RunAgentPy) {
		t.Errorf("served body does not equal embedded run_agent.py "+
			"(got %d bytes, want %d bytes)", len(body), len(client.RunAgentPy))
	}
}

// TestClientRunAgentShapeSanity asserts that the embedded runtime has the
// structural markers CLIENT.md §7 promises: a __VERSION__ constant, a main()
// entry point, the leave_conversation tool name, and the run_agent.py
// banner comment. Catches accidental truncation or wrong-file embeds at
// build time — cheaper than discovering it after deploy.
func TestClientRunAgentShapeSanity(t *testing.T) {
	body := string(client.RunAgentPy)
	if len(body) < 2000 {
		t.Fatalf("embedded run_agent.py suspiciously short: %d bytes", len(body))
	}
	markers := []string{
		"#!/usr/bin/env python3",
		"# run_agent.py",
		"__VERSION__",
		"leave_conversation",
		"def main(",
		"--target",
		"--brief",
		"--register-only",
		// Phase-3 markers — protocol hardening shipped in 2026-04-21.3.
		"NotMemberError",
		"ConversationNotFoundError",
		"TransientError",
		"seed_listen_state",
		"_fetch_orphan",
		"silent_turn",
		"PEERS_LEFT_DEADLINE_SEC",
	}
	// Minimum VERSION the embedded script must be — forces bumping when
	// any of the markers above change.
	if !strings.Contains(body, `__VERSION__ = "2026-04-21.3`) {
		t.Errorf("embedded run_agent.py __VERSION__ is older than 2026-04-21.3; bump both VERSION and the constant")
	}
	for _, m := range markers {
		if !strings.Contains(body, m) {
			t.Errorf("embedded run_agent.py missing marker %q", m)
		}
	}
}
