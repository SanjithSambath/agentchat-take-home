//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agentmail/internal/api"
)

// TestConcurrentInterleavedWrites has two writers stream to the same
// conversation simultaneously. Both message IDs must be cleanly
// demultiplexable on the read side and every delivered seq must be
// strictly monotone.
func TestConcurrentInterleavedWrites(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()

	a := ts.createAgent(t)
	b := ts.createAgent(t)
	reader := ts.createAgent(t)
	conv := ts.createConversation(t, a)
	ts.invite(t, a, conv, b)
	ts.invite(t, a, conv, reader)

	sse := ts.openSSE(t, reader, conv, "")
	defer sse.Close()
	_ = sse.Collect(3, 5*time.Second) // drain 3 agent_joined frames

	chunks := []string{"one ", "two ", "three"}
	var wg sync.WaitGroup
	wg.Add(2)
	for _, sender := range []uuid.UUID{a, b} {
		sender := sender
		go func() {
			defer wg.Done()
			ts.streamMessage(t, sender, conv, chunks)
		}()
	}
	wg.Wait()

	// Per message: start + 3 append + end = 5. Two messages = 10.
	events := sse.Collect(10, 20*time.Second)
	if len(events) != 10 {
		t.Fatalf("expected 10 events, got %d", len(events))
	}

	type group struct {
		starts, appends, ends int
	}
	groups := map[string]*group{}
	var lastSeq uint64
	for i, ev := range events {
		if ev.ID <= lastSeq {
			t.Fatalf("seq non-monotone at %d: %d <= %d", i, ev.ID, lastSeq)
		}
		lastSeq = ev.ID
		m := asMap(t, ev.Data)
		mid, _ := m["message_id"].(string)
		if mid == "" {
			t.Fatalf("missing message_id on event %+v", ev)
		}
		g := groups[mid]
		if g == nil {
			g = &group{}
			groups[mid] = g
		}
		switch ev.Type {
		case "message_start":
			g.starts++
		case "message_append":
			g.appends++
		case "message_end":
			g.ends++
		}
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 message groups, got %d", len(groups))
	}
	for mid, g := range groups {
		if g.starts != 1 || g.appends != 3 || g.ends != 1 {
			t.Fatalf("group %s malformed: %+v", mid, g)
		}
	}
}

// TestStreamingWriteAssembly streams a single message with three chunks
// and asserts the resulting GET /messages history carries a concatenated
// content string (content spans are preserved across chunks).
func TestStreamingWriteAssembly(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()

	a := ts.createAgent(t)
	conv := ts.createConversation(t, a)

	resp := ts.streamMessage(t, a, conv, []string{"alpha ", "beta ", "gamma"})
	if resp.SeqStart == 0 || resp.SeqEnd < resp.SeqStart+4 {
		t.Fatalf("stream response seq window suspicious: %+v", resp)
	}

	var hist api.HistoryResponse
	code, raw := ts.doJSON(t, http.MethodGet,
		fmt.Sprintf("/conversations/%s/messages", conv),
		a, nil, &hist)
	if code != http.StatusOK {
		t.Fatalf("history: status=%d body=%s", code, raw)
	}
	var found bool
	for _, m := range hist.Messages {
		if m.MessageID == resp.MessageID {
			if m.Content != "alpha beta gamma" {
				t.Fatalf("assembled content = %q, want %q", m.Content, "alpha beta gamma")
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("message %s missing from history %+v", resp.MessageID, hist)
	}
}

// TestStreamDedupReplay sends the same message_id twice and expects the
// second response to carry already_processed=true with the same seq span.
func TestStreamDedupReplay(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()

	a := ts.createAgent(t)
	conv := ts.createConversation(t, a)

	msgID, _ := uuid.NewV7()
	body := func() io.Reader {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(api.StreamHeader{MessageID: msgID})
		_ = json.NewEncoder(&buf).Encode(api.StreamChunk{Content: "dedup"})
		return &buf
	}
	doStream := func() api.StreamMessageResponse {
		req, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/conversations/%s/messages/stream", ts.BaseURL, conv),
			body())
		if err != nil {
			t.Fatalf("new req: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		req.Header.Set("X-Agent-ID", a.String())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do req: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream status=%d body=%s", resp.StatusCode, raw)
		}
		var out api.StreamMessageResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode: %v; body=%s", err, raw)
		}
		return out
	}
	first := doStream()
	if first.AlreadyProcessed {
		t.Fatalf("first call already_processed=true: %+v", first)
	}
	second := doStream()
	if !second.AlreadyProcessed {
		t.Fatalf("second call expected already_processed=true: %+v", second)
	}
	if first.SeqStart != second.SeqStart || first.SeqEnd != second.SeqEnd {
		t.Fatalf("replay seq mismatch: first=%+v second=%+v", first, second)
	}
}
