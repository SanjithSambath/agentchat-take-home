package api

import (
	"time"

	"github.com/google/uuid"
)

// Request/response contracts for every JSON endpoint. Shape and field names
// are the wire contract — see ALL_DESIGN_IMPLEMENTATION/http-api-layer-plan.md
// §3 for the canonical table.

// POST /agents

type CreateAgentResponse struct {
	AgentID uuid.UUID `json:"agent_id"`
}

// GET /agents/resident

type GetResidentAgentResponse struct {
	AgentID uuid.UUID `json:"agent_id"`
}

// POST /conversations, GET /conversations

type CreateConversationResponse struct {
	ConversationID uuid.UUID   `json:"conversation_id"`
	Members        []uuid.UUID `json:"members"`
	CreatedAt      time.Time   `json:"created_at"`
}

type ListConversationsResponse struct {
	Conversations []ConversationSummary `json:"conversations"`
}

type ConversationSummary struct {
	ConversationID uuid.UUID   `json:"conversation_id"`
	Members        []uuid.UUID `json:"members"`
	CreatedAt      time.Time   `json:"created_at"`
}

// POST /conversations/{cid}/invite

type InviteRequest struct {
	AgentID uuid.UUID `json:"agent_id"`
}

type InviteResponse struct {
	ConversationID uuid.UUID `json:"conversation_id"`
	AgentID        uuid.UUID `json:"agent_id"`
	AlreadyMember  bool      `json:"already_member"`
}

// POST /conversations/{cid}/leave

type LeaveResponse struct {
	ConversationID uuid.UUID `json:"conversation_id"`
	AgentID        uuid.UUID `json:"agent_id"`
}

// POST /conversations/{cid}/messages (complete send)

type SendMessageRequest struct {
	MessageID uuid.UUID `json:"message_id"`
	Content   string    `json:"content"`
}

type SendMessageResponse struct {
	MessageID        uuid.UUID `json:"message_id"`
	SeqStart         uint64    `json:"seq_start"`
	SeqEnd           uint64    `json:"seq_end"`
	AlreadyProcessed bool      `json:"already_processed"`
}

// POST /conversations/{cid}/messages/stream (streaming send)

type StreamHeader struct {
	MessageID uuid.UUID `json:"message_id"`
}

type StreamChunk struct {
	Content string `json:"content"`
}

type StreamMessageResponse struct {
	MessageID        uuid.UUID `json:"message_id"`
	SeqStart         uint64    `json:"seq_start"`
	SeqEnd           uint64    `json:"seq_end"`
	AlreadyProcessed bool      `json:"already_processed"`
}

// GET /conversations/{cid}/messages (history)

type HistoryResponse struct {
	Messages []HistoryMessage `json:"messages"`
	HasMore  bool             `json:"has_more"`
}

// HistoryMessage mirrors model.HistoryMessage but lives in the api package so
// callers never import the model package solely for the wire type. JSON tags
// match the frozen model shape exactly.
type HistoryMessage struct {
	MessageID uuid.UUID `json:"message_id"`
	SenderID  uuid.UUID `json:"sender_id"`
	Content   string    `json:"content"`
	SeqStart  uint64    `json:"seq_start"`
	SeqEnd    uint64    `json:"seq_end"`
	Status    string    `json:"status"`
}

// POST /conversations/{cid}/ack

type AckRequest struct {
	Seq uint64 `json:"seq"`
}

// GET /agents/me/unread

type UnreadResponse struct {
	Conversations []UnreadEntry `json:"conversations"`
}

type UnreadEntry struct {
	ConversationID uuid.UUID `json:"conversation_id"`
	HeadSeq        uint64    `json:"head_seq"`
	AckSeq         uint64    `json:"ack_seq"`
	EventDelta     uint64    `json:"event_delta"`
}
