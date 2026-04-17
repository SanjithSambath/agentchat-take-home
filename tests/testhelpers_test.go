//go:build integration

// Package tests is the end-to-end integration harness. Every test in this
// package spins up a real HTTP server backed by live Neon (Postgres) and S2
// (streamstore) — the same wiring main.go uses in production — and hits it
// over loopback.
//
// Run with:
//
//	DATABASE_URL=… S2_AUTH_TOKEN=… make test-integration
//
// Tests are skipped automatically when the required env vars are absent, so
// `go test ./...` without the integration tag stays clean.
package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/uuid"

	"agentmail/internal/agent"
	"agentmail/internal/api"
	"agentmail/internal/config"
	"agentmail/internal/store"
)

// requireLiveInfra skips the test unless DATABASE_URL and S2_AUTH_TOKEN (or
// S2_ACCESS_TOKEN) are set. Returns the values so the caller can build a
// config.Config without re-reading the environment.
func requireLiveInfra(t *testing.T) config.Config {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration: DATABASE_URL not set")
	}
	tok := os.Getenv("S2_AUTH_TOKEN")
	if tok == "" {
		tok = os.Getenv("S2_ACCESS_TOKEN")
	}
	if tok == "" {
		t.Skip("integration: S2_AUTH_TOKEN (or S2_ACCESS_TOKEN) not set")
	}
	basin := os.Getenv("S2_BASIN")
	if basin == "" {
		basin = "agentmail"
	}
	return config.Config{
		DatabaseURL:         dsn,
		S2AuthToken:         tok,
		S2Basin:             basin,
		CursorFlushInterval: 1 * time.Second,
		HealthCheckTimeout:  5 * time.Second,
	}
}

// testServer bundles the running HTTP server plus every backing dependency
// so tests can inject at any layer (agent, meta, s2) without reflection.
type testServer struct {
	BaseURL string
	Meta    store.MetadataStore
	S2      store.S2Store
	Resident *agent.Agent // nil unless RESIDENT_AGENT_ID is configured

	srv  *httptest.Server
	stop func()
}

// startTestServer wires real Postgres + real S2 + the production router into
// an httptest.Server listening on 127.0.0.1:<ephemeral>. The returned server
// must be closed with Close().
//
// withResident controls whether the resident Claude agent is bootstrapped.
// Requires RESIDENT_AGENT_ID + ANTHROPIC_API_KEY when true; skips the test
// otherwise.
func startTestServer(t *testing.T, withResident bool) *testServer {
	t.Helper()
	cfg := requireLiveInfra(t)

	if withResident {
		cfg.AnthropicAPIKey = os.Getenv("ANTHROPIC_API_KEY")
		if cfg.AnthropicAPIKey == "" {
			t.Skip("integration (resident): ANTHROPIC_API_KEY not set")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	meta, err := store.NewMetadataStoreWithOptions(ctx, cfg.DatabaseURL, store.MetadataStoreOptions{
		CursorFlushInterval: cfg.CursorFlushInterval,
	})
	if err != nil {
		cancel()
		t.Fatalf("integration: metadata store: %v", err)
	}
	meta.StartDeliveryCursorFlusher(ctx)

	s2, err := store.NewS2Store(cfg)
	if err != nil {
		_ = meta.Close()
		cancel()
		t.Fatalf("integration: s2 store: %v", err)
	}

	// Mint a fresh agent row to act as the resident. MetadataStore has no
	// public way to upsert an operator-supplied id, so tests use whatever
	// CreateAgent returns. The agent's registerIdentity is happy with any
	// id that exists in the agents table.
	if withResident {
		rid, err := meta.CreateAgent(ctx)
		if err != nil {
			_ = meta.Close()
			cancel()
			t.Fatalf("integration: mint resident agent: %v", err)
		}
		cfg.ResidentAgentID = rid
	}

	var resident api.ResidentInfo = api.DisabledResident{}
	var agentInst *agent.Agent
	if withResident {
		client := anthropic.NewClient(option.WithAPIKey(cfg.AnthropicAPIKey))
		a, err := agent.NewAgent(cfg, s2, meta, &client)
		if err != nil {
			_ = meta.Close()
			cancel()
			t.Fatalf("integration: agent construct: %v", err)
		}
		startCtx, startCancel := context.WithTimeout(ctx, 30*time.Second)
		defer startCancel()
		if err := a.Start(startCtx); err != nil {
			_ = meta.Close()
			cancel()
			t.Fatalf("integration: agent start: %v", err)
		}
		agentInst = a
		resident = a
	}

	conns := api.NewConnRegistry()
	router := api.NewRouter(s2, meta, resident, conns)

	srv := httptest.NewServer(router)

	ts := &testServer{
		BaseURL:  srv.URL,
		Meta:     meta,
		S2:       s2,
		Resident: agentInst,
		srv:      srv,
	}
	ts.stop = func() {
		srv.Close()
		if agentInst != nil {
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
			agentInst.Shutdown(shutCtx)
			shutCancel()
		}
		_ = meta.Close()
		cancel()
	}
	return ts
}

func (ts *testServer) Close() {
	if ts != nil && ts.stop != nil {
		ts.stop()
	}
}

// doJSON performs a JSON HTTP request and decodes the response body into
// `out` iff non-nil and the status is 2xx. Returns the status code and the
// raw body for error introspection.
func (ts *testServer) doJSON(t *testing.T, method, path string, agentID uuid.UUID, body, out any) (int, []byte) {
	t.Helper()
	return ts.doRequest(t, method, path, agentID, body, out)
}

func (ts *testServer) doRequest(t *testing.T, method, path string, agentID uuid.UUID, body, out any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, ts.BaseURL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if agentID != uuid.Nil {
		req.Header.Set("X-Agent-ID", agentID.String())
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode response %s %s: %v; body=%s", method, path, err, raw)
		}
	}
	return resp.StatusCode, raw
}

// createAgent registers a fresh agent and returns its UUID.
func (ts *testServer) createAgent(t *testing.T) uuid.UUID {
	t.Helper()
	var resp api.CreateAgentResponse
	code, raw := ts.doJSON(t, http.MethodPost, "/agents", uuid.Nil, struct{}{}, &resp)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create agent: status=%d body=%s", code, raw)
	}
	return resp.AgentID
}

// createConversation creates a conversation owned by `creator` and returns
// its UUID.
func (ts *testServer) createConversation(t *testing.T, creator uuid.UUID) uuid.UUID {
	t.Helper()
	var resp api.CreateConversationResponse
	code, raw := ts.doJSON(t, http.MethodPost, "/conversations", creator, struct{}{}, &resp)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create conversation: status=%d body=%s", code, raw)
	}
	return resp.ConversationID
}

// invite adds `invitee` to a conversation on behalf of `inviter`.
func (ts *testServer) invite(t *testing.T, inviter, conv, invitee uuid.UUID) api.InviteResponse {
	t.Helper()
	var resp api.InviteResponse
	code, raw := ts.doJSON(t, http.MethodPost,
		fmt.Sprintf("/conversations/%s/invite", conv),
		inviter,
		api.InviteRequest{AgentID: invitee},
		&resp,
	)
	if code != http.StatusOK {
		t.Fatalf("invite: status=%d body=%s", code, raw)
	}
	return resp
}

// sendMessage issues a complete (non-streaming) POST /messages and returns
// its seq_start / seq_end.
func (ts *testServer) sendMessage(t *testing.T, sender, conv uuid.UUID, content string) api.SendMessageResponse {
	t.Helper()
	msgID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint message id: %v", err)
	}
	var resp api.SendMessageResponse
	code, raw := ts.doJSON(t, http.MethodPost,
		fmt.Sprintf("/conversations/%s/messages", conv),
		sender,
		api.SendMessageRequest{MessageID: msgID, Content: content},
		&resp,
	)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("send message: status=%d body=%s", code, raw)
	}
	return resp
}

// streamMessage streams an NDJSON write — header + one append per chunk +
// EOF triggers message_end. Returns the server's final response.
func (ts *testServer) streamMessage(t *testing.T, sender, conv uuid.UUID, chunks []string) api.StreamMessageResponse {
	t.Helper()
	msgID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint message id: %v", err)
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(api.StreamHeader{MessageID: msgID}); err != nil {
		t.Fatalf("encode header: %v", err)
	}
	for _, c := range chunks {
		if err := json.NewEncoder(&body).Encode(api.StreamChunk{Content: c}); err != nil {
			t.Fatalf("encode chunk: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPost,
		ts.BaseURL+fmt.Sprintf("/conversations/%s/messages/stream", conv), &body)
	if err != nil {
		t.Fatalf("new stream request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("X-Agent-ID", sender.String())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do stream request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: status=%d body=%s", resp.StatusCode, raw)
	}
	var out api.StreamMessageResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode stream response: %v; body=%s", err, raw)
	}
	return out
}

// sseClient is a minimal SSE consumer. Consumers loop over Events() until
// Close() is called (or the upstream closes). Events are parsed from the
// `id:/event:/data:` frames.
type sseEvent struct {
	ID    uint64
	Type  string
	Data  json.RawMessage
}

type sseClient struct {
	resp   *http.Response
	events chan sseEvent
	errs   chan error
	done   chan struct{}
	cancel context.CancelFunc
	once   sync.Once
}

func (ts *testServer) openSSE(t *testing.T, agentID, convID uuid.UUID, lastEventID string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx,
		http.MethodGet,
		ts.BaseURL+fmt.Sprintf("/conversations/%s/stream", convID),
		nil)
	if err != nil {
		cancel()
		t.Fatalf("new sse request: %v", err)
	}
	req.Header.Set("X-Agent-ID", agentID.String())
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("do sse request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		t.Fatalf("sse: status=%d body=%s", resp.StatusCode, raw)
	}
	c := &sseClient{
		resp:   resp,
		events: make(chan sseEvent, 64),
		errs:   make(chan error, 1),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	go c.read()
	return c
}

func (c *sseClient) read() {
	defer close(c.done)
	defer close(c.events)
	br := bufio.NewReader(c.resp.Body)
	var id uint64
	var evType string
	var dataBuf bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				select {
				case c.errs <- err:
				default:
				}
			}
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// Frame boundary — emit.
			if evType != "" && dataBuf.Len() > 0 {
				select {
				case c.events <- sseEvent{ID: id, Type: evType, Data: json.RawMessage(bytes.Clone(dataBuf.Bytes()))}:
				case <-c.done:
					return
				}
			}
			id, evType = 0, ""
			dataBuf.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment (heartbeat)
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			v, err := strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
			if err == nil {
				id = v
			}
		case strings.HasPrefix(line, "event: "):
			evType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
}

// Next waits up to `d` for the next event. Returns (event, true) on success,
// (zero, false) on timeout or stream close.
func (c *sseClient) Next(d time.Duration) (sseEvent, bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case ev, ok := <-c.events:
		if !ok {
			return sseEvent{}, false
		}
		return ev, true
	case <-t.C:
		return sseEvent{}, false
	}
}

// Collect drains events into a slice until either the predicate returns
// true or `total` is reached or the deadline fires.
func (c *sseClient) Collect(total int, d time.Duration) []sseEvent {
	deadline := time.Now().Add(d)
	out := make([]sseEvent, 0, total)
	for len(out) < total {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return out
		}
		ev, ok := c.Next(remaining)
		if !ok {
			return out
		}
		out = append(out, ev)
	}
	return out
}

func (c *sseClient) Close() {
	c.once.Do(func() {
		c.cancel()
		_ = c.resp.Body.Close()
		<-c.done
	})
}

// asJSONString unmarshals an SSE data payload into a generic map for
// assertions that only need one or two fields.
func asMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode event data: %v; raw=%s", err, raw)
	}
	return m
}

// ensureLoopback guards against accidentally hitting the wrong host if BaseURL
// gets overridden by a proxy.
func ensureLoopback(t *testing.T, u string) {
	t.Helper()
	if !strings.Contains(u, "127.0.0.1") {
		t.Fatalf("expected loopback base url, got %s", u)
	}
	if _, _, err := net.SplitHostPort(strings.TrimPrefix(u, "http://")); err != nil {
		t.Fatalf("parse base url host: %v", err)
	}
}
