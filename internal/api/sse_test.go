package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// sseRouter builds a chi router with the SSE and send-message routes plus an
// AgentAuth-like middleware that reads X-Agent-ID from the request.
func sseRouter(t *testing.T, h *Handler, meta *fakeMeta) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			raw := req.Header.Get("X-Agent-ID")
			id, err := uuid.Parse(raw)
			if err != nil {
				http.Error(w, "bad agent", http.StatusBadRequest)
				return
			}
			ok, _ := meta.AgentExists(req.Context(), id)
			if !ok {
				http.Error(w, "unknown agent", http.StatusNotFound)
				return
			}
			next.ServeHTTP(w, req.WithContext(WithAgentID(req.Context(), id)))
		})
	})
	r.Post("/conversations", h.CreateConversation)
	r.Post("/conversations/{cid}/messages", h.SendMessage)
	r.Get("/conversations/{cid}/stream", h.SSEStream)
	return r
}

func TestSSE_ReceivesEvents(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)

	srv := httptest.NewServer(sseRouter(t, h, meta))
	defer srv.Close()

	// Create conversation as A.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/conversations", nil)
	req.Header.Set("X-Agent-ID", a.String())
	createRes, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create conv: %v", err)
	}
	var conv CreateConversationResponse
	if err := decodeResp(createRes, &conv); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Open SSE.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet,
		srv.URL+"/conversations/"+conv.ConversationID.String()+"/stream?from=1", nil)
	sseReq.Header.Set("X-Agent-ID", a.String())
	sseRes, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("sse open: %v", err)
	}
	defer sseRes.Body.Close()
	if sseRes.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d", sseRes.StatusCode)
	}

	// Send a message in a goroutine.
	go func() {
		time.Sleep(50 * time.Millisecond)
		msgID := uuid.Must(uuid.NewV7())
		body := fmt.Sprintf(`{"message_id":"%s","content":"hello"}`, msgID)
		req, _ := http.NewRequest(http.MethodPost,
			srv.URL+"/conversations/"+conv.ConversationID.String()+"/messages",
			strings.NewReader(body))
		req.Header.Set("X-Agent-ID", a.String())
		req.Header.Set("Content-Type", "application/json")
		_, _ = http.DefaultClient.Do(req)
	}()

	// Read SSE frames until we see a message_start, message_append, message_end.
	reader := bufio.NewReader(sseRes.Body)
	events := readSSEEvents(t, reader, 4, 3*time.Second) // agent_joined + 3 msg events
	sseCancel()

	kinds := map[string]int{}
	for _, e := range events {
		kinds[e.event]++
	}
	if kinds["message_start"] == 0 || kinds["message_append"] == 0 || kinds["message_end"] == 0 {
		t.Fatalf("missing message events; got kinds=%v", kinds)
	}
}

type sseEvent struct {
	id    string
	event string
	data  string
}

func readSSEEvents(t *testing.T, r *bufio.Reader, want int, timeout time.Duration) []sseEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var out []sseEvent
	var cur sseEvent
	for len(out) < want {
		if time.Now().After(deadline) {
			break
		}
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			t.Fatalf("read sse: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if cur.event != "" || cur.data != "" {
				out = append(out, cur)
				cur = sseEvent{}
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / heartbeat
		}
		switch {
		case strings.HasPrefix(line, "id:"):
			cur.id = strings.TrimSpace(line[3:])
		case strings.HasPrefix(line, "event:"):
			cur.event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			cur.data = strings.TrimSpace(line[5:])
		}
	}
	return out
}

func decodeResp(res *http.Response, v any) error {
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("status=%d body=%s", res.StatusCode, body)
	}
	return jsonDecode(res.Body, v)
}
