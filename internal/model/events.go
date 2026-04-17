// Package model defines the frozen event types that flow through the S2 stream.
//
// These types are the wire format between every layer: the write path produces
// Event values that the S2 wrapper serializes to records; the read path produces
// SequencedEvent values that the SSE handler, history handler, and Claude agent
// consume. Changing these types changes the wire format; treat this file as a
// contract.
//
// See ALL_DESIGN_IMPLEMENTATION/event-model-plan.md for the full rationale.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// EventType is the event kind. It is carried in the S2 record header (not the
// body) so consumers can dispatch without parsing JSON.
type EventType string

const (
	EventTypeMessageStart  EventType = "message_start"
	EventTypeMessageAppend EventType = "message_append"
	EventTypeMessageEnd    EventType = "message_end"
	EventTypeMessageAbort  EventType = "message_abort"
	EventTypeAgentJoined   EventType = "agent_joined"
	EventTypeAgentLeft     EventType = "agent_left"
)

// Abort reasons. Descriptive metadata; not a programmatic branch point.
const (
	AbortReasonDisconnect      = "disconnect"
	AbortReasonServerCrash     = "server_crash"
	AbortReasonAgentLeft       = "agent_left"
	AbortReasonClaudeError     = "claude_error"
	AbortReasonIdleTimeout     = "idle_timeout"
	AbortReasonMaxDuration     = "max_duration"
	AbortReasonLineTooLarge    = "line_too_large"
	AbortReasonContentTooLarge = "content_too_large"
	AbortReasonInvalidUTF8     = "invalid_utf8"
	AbortReasonSlowWriter      = "slow_writer"
)

// Assembly status for reconstructed messages. Used by the history endpoint and
// Claude's history seeding.
const (
	StatusComplete   = "complete"
	StatusAborted    = "aborted"
	StatusInProgress = "in_progress"
)

// --- Payload structs (one per event kind) --------------------------------

type MessageStartPayload struct {
	MessageID uuid.UUID `json:"message_id"`
	SenderID  uuid.UUID `json:"sender_id"`
}

type MessageAppendPayload struct {
	MessageID uuid.UUID `json:"message_id"`
	Content   string    `json:"content"`
}

type MessageEndPayload struct {
	MessageID uuid.UUID `json:"message_id"`
}

type MessageAbortPayload struct {
	MessageID uuid.UUID `json:"message_id"`
	Reason    string    `json:"reason"`
}

type AgentJoinedPayload struct {
	AgentID uuid.UUID `json:"agent_id"`
}

type AgentLeftPayload struct {
	AgentID uuid.UUID `json:"agent_id"`
}

// --- Write path type -----------------------------------------------------

// Event is the write-path value. Type goes into the S2 record header, Body is
// marshaled to the record body. Constructed via one of the New* functions.
type Event struct {
	Type EventType
	Body any
}

// MarshalBody returns the JSON-encoded body suitable for the S2 record body.
func (e Event) MarshalBody() ([]byte, error) {
	return json.Marshal(e.Body)
}

// --- Constructors --------------------------------------------------------
//
// Constructors panic on obviously-broken input (nil UUIDs, empty content on
// a complete-message batch). These are programmer errors — the HTTP layer
// must validate before reaching the constructor.

func NewMessageStartEvent(messageID, senderID uuid.UUID) Event {
	if messageID == uuid.Nil {
		panic("model: NewMessageStartEvent called with nil messageID")
	}
	if senderID == uuid.Nil {
		panic("model: NewMessageStartEvent called with nil senderID")
	}
	return Event{Type: EventTypeMessageStart, Body: MessageStartPayload{MessageID: messageID, SenderID: senderID}}
}

func NewMessageAppendEvent(messageID uuid.UUID, content string) Event {
	if messageID == uuid.Nil {
		panic("model: NewMessageAppendEvent called with nil messageID")
	}
	return Event{Type: EventTypeMessageAppend, Body: MessageAppendPayload{MessageID: messageID, Content: content}}
}

func NewMessageEndEvent(messageID uuid.UUID) Event {
	if messageID == uuid.Nil {
		panic("model: NewMessageEndEvent called with nil messageID")
	}
	return Event{Type: EventTypeMessageEnd, Body: MessageEndPayload{MessageID: messageID}}
}

func NewMessageAbortEvent(messageID uuid.UUID, reason string) Event {
	if messageID == uuid.Nil {
		panic("model: NewMessageAbortEvent called with nil messageID")
	}
	return Event{Type: EventTypeMessageAbort, Body: MessageAbortPayload{MessageID: messageID, Reason: reason}}
}

func NewAgentJoinedEvent(agentID uuid.UUID) Event {
	if agentID == uuid.Nil {
		panic("model: NewAgentJoinedEvent called with nil agentID")
	}
	return Event{Type: EventTypeAgentJoined, Body: AgentJoinedPayload{AgentID: agentID}}
}

func NewAgentLeftEvent(agentID uuid.UUID) Event {
	if agentID == uuid.Nil {
		panic("model: NewAgentLeftEvent called with nil agentID")
	}
	return Event{Type: EventTypeAgentLeft, Body: AgentLeftPayload{AgentID: agentID}}
}

// NewCompleteMessageEvents produces the 3-record batch for a complete message
// send: [message_start, message_append, message_end]. Written atomically as
// one Unary append.
func NewCompleteMessageEvents(messageID, senderID uuid.UUID, content string) []Event {
	if content == "" {
		panic("model: NewCompleteMessageEvents called with empty content")
	}
	return []Event{
		NewMessageStartEvent(messageID, senderID),
		NewMessageAppendEvent(messageID, content),
		NewMessageEndEvent(messageID),
	}
}

// --- Read path type ------------------------------------------------------

// SequencedEvent is the read-path value. SeqNum and Timestamp are S2-assigned.
// Data is the raw JSON body — NOT pre-parsed. The SSE hot path passes Data
// through untouched; history and Claude parse lazily via ParsePayload.
type SequencedEvent struct {
	SeqNum    uint64
	Timestamp time.Time
	Type      EventType
	Data      json.RawMessage
}

// ParsePayload decodes the raw JSON body of a SequencedEvent into a typed
// payload struct. Example: ParsePayload[MessageStartPayload](ev).
func ParsePayload[T any](se SequencedEvent) (T, error) {
	var p T
	if err := json.Unmarshal(se.Data, &p); err != nil {
		return p, fmt.Errorf("model: parse %s payload (seq %d): %w", se.Type, se.SeqNum, err)
	}
	return p, nil
}

// --- SSE enrichment ------------------------------------------------------

// EnrichForSSE injects a `timestamp` field into the event body for SSE output.
// Timestamps are not stored in payloads — they're S2-assigned and added here
// at render time.
func EnrichForSSE(data json.RawMessage, timestamp time.Time) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return data, err
	}
	tsJSON, err := json.Marshal(timestamp.UTC().Format(time.RFC3339))
	if err != nil {
		return data, err
	}
	m["timestamp"] = tsJSON
	return json.Marshal(m)
}

// --- Message assembly ----------------------------------------------------

// HistoryMessage is an assembled complete message, produced by AssembleMessages
// from a window of SequencedEvents. Used by the history endpoint and Claude's
// history seeding.
type HistoryMessage struct {
	MessageID uuid.UUID `json:"message_id"`
	SenderID  uuid.UUID `json:"sender_id"`
	Content   string    `json:"content"`
	SeqStart  uint64    `json:"seq_start"`
	SeqEnd    uint64    `json:"seq_end"`
	Status    string    `json:"status"`
}

type pendingAssembly struct {
	messageID uuid.UUID
	senderID  uuid.UUID
	content   strings.Builder
	seqStart  uint64
}

// AssembleMessages groups SequencedEvents by message_id and returns assembled
// HistoryMessages. Events for messages that never saw a message_end are
// returned with Status = in_progress and SeqEnd = 0.
func AssembleMessages(events []SequencedEvent) []HistoryMessage {
	pending := make(map[uuid.UUID]*pendingAssembly)
	var messages []HistoryMessage

	for _, event := range events {
		switch event.Type {
		case EventTypeMessageStart:
			payload, err := ParsePayload[MessageStartPayload](event)
			if err != nil {
				continue
			}
			pending[payload.MessageID] = &pendingAssembly{
				messageID: payload.MessageID,
				senderID:  payload.SenderID,
				seqStart:  event.SeqNum,
			}

		case EventTypeMessageAppend:
			payload, err := ParsePayload[MessageAppendPayload](event)
			if err != nil {
				continue
			}
			p, ok := pending[payload.MessageID]
			if !ok {
				continue
			}
			p.content.WriteString(payload.Content)

		case EventTypeMessageEnd:
			payload, err := ParsePayload[MessageEndPayload](event)
			if err != nil {
				continue
			}
			p, ok := pending[payload.MessageID]
			if !ok {
				continue
			}
			delete(pending, payload.MessageID)
			messages = append(messages, HistoryMessage{
				MessageID: p.messageID,
				SenderID:  p.senderID,
				Content:   p.content.String(),
				SeqStart:  p.seqStart,
				SeqEnd:    event.SeqNum,
				Status:    StatusComplete,
			})

		case EventTypeMessageAbort:
			payload, err := ParsePayload[MessageAbortPayload](event)
			if err != nil {
				continue
			}
			p, ok := pending[payload.MessageID]
			if !ok {
				continue
			}
			delete(pending, payload.MessageID)
			messages = append(messages, HistoryMessage{
				MessageID: p.messageID,
				SenderID:  p.senderID,
				Content:   p.content.String(),
				SeqStart:  p.seqStart,
				SeqEnd:    event.SeqNum,
				Status:    StatusAborted,
			})
		}
	}

	for _, p := range pending {
		messages = append(messages, HistoryMessage{
			MessageID: p.messageID,
			SenderID:  p.senderID,
			Content:   p.content.String(),
			SeqStart:  p.seqStart,
			SeqEnd:    0,
			Status:    StatusInProgress,
		})
	}

	return messages
}
