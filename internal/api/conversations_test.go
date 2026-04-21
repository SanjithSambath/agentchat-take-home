package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// routeHelper wires the Handler into a minimal chi router so path params and
// context work as they would under NewRouter. The AgentAuth middleware is
// skipped; tests inject the agent id directly via WithAgentID.
func routeHelper(t *testing.T, h *Handler, agentID uuid.UUID) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := WithAgentID(req.Context(), agentID)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Post("/conversations", h.CreateConversation)
	r.Get("/conversations", h.ListConversations)
	r.Get("/conversations/{cid}", h.GetConversation)
	r.Post("/conversations/{cid}/invite", h.InviteAgent)
	r.Post("/conversations/{cid}/leave", h.LeaveConversation)
	r.Post("/conversations/{cid}/messages", h.SendMessage)
	r.Post("/conversations/{cid}/messages/stream", h.StreamMessage)
	r.Get("/conversations/{cid}/messages", h.GetHistory)
	r.Get("/conversations/{cid}/stream", h.SSEStream)
	r.Post("/conversations/{cid}/ack", h.AckCursor)
	r.Get("/agents/me/unread", h.ListUnread)
	return r
}

func createConv(t *testing.T, r chi.Router) uuid.UUID {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/conversations", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create conversation: %d %s", rec.Code, rec.Body.String())
	}
	var body CreateConversationResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.ConversationID
}

func TestCreateConversation(t *testing.T) {
	h, meta, _ := newTestHandler()
	agentID := registerAgent(t, meta)
	r := routeHelper(t, h, agentID)

	convID := createConv(t, r)
	if convID == uuid.Nil {
		t.Fatal("nil conv id")
	}
	members, _ := meta.ListMembers(context.Background(), convID)
	if len(members) != 1 || members[0] != agentID {
		t.Fatalf("members = %v; want [%s]", members, agentID)
	}
}

func TestGetConversation(t *testing.T) {
	h, meta, _ := newTestHandler()
	creator := registerAgent(t, meta)
	invitee := registerAgent(t, meta)
	stranger := registerAgent(t, meta)

	// Creator router: creates conv + invites.
	rCreator := routeHelper(t, h, creator)
	convID := createConv(t, rCreator)
	inviteBuf, _ := json.Marshal(InviteRequest{AgentID: invitee})
	invReq := httptest.NewRequest(http.MethodPost,
		"/conversations/"+convID.String()+"/invite", bytes.NewReader(inviteBuf))
	invReq.Header.Set("Content-Type", "application/json")
	invRec := httptest.NewRecorder()
	rCreator.ServeHTTP(invRec, invReq)
	if invRec.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", invRec.Code, invRec.Body.String())
	}

	// Creator can GET — expect creator + invitee in members.
	rec := httptest.NewRecorder()
	rCreator.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/conversations/"+convID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("creator GET: %d %s", rec.Code, rec.Body.String())
	}
	var body ConversationSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if body.ConversationID != convID {
		t.Errorf("conversation_id = %s; want %s", body.ConversationID, convID)
	}
	gotMembers := map[uuid.UUID]bool{}
	for _, m := range body.Members {
		gotMembers[m] = true
	}
	if !gotMembers[creator] || !gotMembers[invitee] {
		t.Errorf("members = %v; want both %s and %s", body.Members, creator, invitee)
	}
	if body.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}

	// Invitee can GET too — they're a member after the invite.
	rInvitee := routeHelper(t, h, invitee)
	rec = httptest.NewRecorder()
	rInvitee.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/conversations/"+convID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("invitee GET: %d %s", rec.Code, rec.Body.String())
	}

	// Non-member — 403 not_member (prevents membership snooping via GET).
	rStranger := routeHelper(t, h, stranger)
	rec = httptest.NewRecorder()
	rStranger.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/conversations/"+convID.String(), nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("stranger GET: %d %s; want 403", rec.Code, rec.Body.String())
	}

	// Unknown conversation — 404 conversation_not_found.
	rec = httptest.NewRecorder()
	rCreator.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/conversations/"+uuid.New().String(), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown conv GET: %d %s; want 404", rec.Code, rec.Body.String())
	}
}

func TestInviteAgent(t *testing.T) {
	h, meta, _ := newTestHandler()
	creator := registerAgent(t, meta)
	invitee := registerAgent(t, meta)
	r := routeHelper(t, h, creator)
	convID := createConv(t, r)

	post := func(body any) *httptest.ResponseRecorder {
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/conversations/"+convID.String()+"/invite",
			bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// Happy path.
	rec := post(InviteRequest{AgentID: invitee})
	if rec.Code != http.StatusOK {
		t.Fatalf("first invite: %d %s", rec.Code, rec.Body.String())
	}
	var first InviteResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	if first.AlreadyMember {
		t.Fatal("first invite reported already_member")
	}

	// Idempotent re-invite.
	rec = post(InviteRequest{AgentID: invitee})
	if rec.Code != http.StatusOK {
		t.Fatalf("second invite: %d %s", rec.Code, rec.Body.String())
	}
	var second InviteResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	if !second.AlreadyMember {
		t.Fatal("second invite should report already_member")
	}

	// Unknown invitee → 404.
	rec = post(InviteRequest{AgentID: uuid.New()})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown invitee status = %d", rec.Code)
	}
}

func TestLeaveConversation(t *testing.T) {
	h, meta, _ := newTestHandler()
	a := registerAgent(t, meta)
	b := registerAgent(t, meta)
	rA := routeHelper(t, h, a)
	convID := createConv(t, rA)

	// Invite B so A can leave without tripping last_member.
	body, _ := json.Marshal(InviteRequest{AgentID: b})
	req := httptest.NewRequest(http.MethodPost, "/conversations/"+convID.String()+"/invite", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rA.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body.String())
	}

	// A leaves — should succeed.
	req = httptest.NewRequest(http.MethodPost, "/conversations/"+convID.String()+"/leave", nil)
	rec = httptest.NewRecorder()
	rA.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	// B (only remaining) tries to leave — last_member 409.
	rB := routeHelper(t, h, b)
	req = httptest.NewRequest(http.MethodPost, "/conversations/"+convID.String()+"/leave", nil)
	rec = httptest.NewRecorder()
	rB.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("last-member leave: %d %s", rec.Code, rec.Body.String())
	}
	var env APIError
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != CodeLastMember {
		t.Fatalf("code = %q, want %q", env.Error.Code, CodeLastMember)
	}
}

func TestRequireMembership_NotMember(t *testing.T) {
	h, meta, _ := newTestHandler()
	creator := registerAgent(t, meta)
	outsider := registerAgent(t, meta)
	r := routeHelper(t, h, creator)
	convID := createConv(t, r)

	rOut := routeHelper(t, h, outsider)
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/conversations/%s/messages", convID), nil)
	rec := httptest.NewRecorder()
	rOut.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}
