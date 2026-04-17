package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestConstructorsProduceCorrectTypes(t *testing.T) {
	mid := uuid.New()
	sid := uuid.New()

	cases := []struct {
		name    string
		event   Event
		wantTyp EventType
	}{
		{"start", NewMessageStartEvent(mid, sid), EventTypeMessageStart},
		{"append", NewMessageAppendEvent(mid, "hello"), EventTypeMessageAppend},
		{"end", NewMessageEndEvent(mid), EventTypeMessageEnd},
		{"abort", NewMessageAbortEvent(mid, AbortReasonDisconnect), EventTypeMessageAbort},
		{"joined", NewAgentJoinedEvent(sid), EventTypeAgentJoined},
		{"left", NewAgentLeftEvent(sid), EventTypeAgentLeft},
	}
	for _, tc := range cases {
		t.Run(string(tc.name), func(t *testing.T) {
			if tc.event.Type != tc.wantTyp {
				t.Errorf("got type %q, want %q", tc.event.Type, tc.wantTyp)
			}
			if _, err := tc.event.MarshalBody(); err != nil {
				t.Errorf("MarshalBody: %v", err)
			}
		})
	}
}

func TestCompleteMessageBatch(t *testing.T) {
	mid := uuid.New()
	sid := uuid.New()
	events := NewCompleteMessageEvents(mid, sid, "hello world")
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d", len(events))
	}
	if events[0].Type != EventTypeMessageStart ||
		events[1].Type != EventTypeMessageAppend ||
		events[2].Type != EventTypeMessageEnd {
		t.Errorf("wrong event order: %v %v %v", events[0].Type, events[1].Type, events[2].Type)
	}
}

func TestRoundTripThroughSequencedEvent(t *testing.T) {
	mid := uuid.New()
	sid := uuid.New()
	ev := NewMessageStartEvent(mid, sid)
	body, err := ev.MarshalBody()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	se := SequencedEvent{
		SeqNum:    42,
		Timestamp: time.Now().UTC(),
		Type:      ev.Type,
		Data:      body,
	}
	p, err := ParsePayload[MessageStartPayload](se)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.MessageID != mid || p.SenderID != sid {
		t.Errorf("round-trip mismatch: got %+v, want mid=%s sid=%s", p, mid, sid)
	}
}

func TestEnrichForSSEInjectsTimestamp(t *testing.T) {
	body, _ := json.Marshal(MessageEndPayload{MessageID: uuid.New()})
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	enriched, err := EnrichForSSE(body, ts)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(enriched, &m); err != nil {
		t.Fatalf("unmarshal enriched: %v", err)
	}
	if got, want := m["timestamp"], "2026-01-02T03:04:05Z"; got != want {
		t.Errorf("timestamp: got %v, want %q", got, want)
	}
}

func TestAssembleMessagesHandlesCompleteAbortInProgress(t *testing.T) {
	sid := uuid.New()
	mid1, mid2, mid3 := uuid.New(), uuid.New(), uuid.New()

	build := func(seq uint64, ev Event) SequencedEvent {
		b, _ := ev.MarshalBody()
		return SequencedEvent{SeqNum: seq, Timestamp: time.Unix(int64(seq), 0), Type: ev.Type, Data: b}
	}

	stream := []SequencedEvent{
		build(1, NewMessageStartEvent(mid1, sid)),
		build(2, NewMessageAppendEvent(mid1, "hello ")),
		build(3, NewMessageAppendEvent(mid1, "world")),
		build(4, NewMessageEndEvent(mid1)),
		build(5, NewMessageStartEvent(mid2, sid)),
		build(6, NewMessageAppendEvent(mid2, "partial ")),
		build(7, NewMessageAbortEvent(mid2, AbortReasonDisconnect)),
		build(8, NewMessageStartEvent(mid3, sid)),
		build(9, NewMessageAppendEvent(mid3, "still typing")),
	}

	out := AssembleMessages(stream)
	if len(out) != 3 {
		t.Fatalf("want 3 messages, got %d", len(out))
	}

	byID := map[uuid.UUID]HistoryMessage{}
	for _, m := range out {
		byID[m.MessageID] = m
	}
	if m := byID[mid1]; m.Status != StatusComplete || m.Content != "hello world" {
		t.Errorf("mid1: want complete 'hello world', got %s %q", m.Status, m.Content)
	}
	if m := byID[mid2]; m.Status != StatusAborted || m.Content != "partial " {
		t.Errorf("mid2: want aborted 'partial ', got %s %q", m.Status, m.Content)
	}
	if m := byID[mid3]; m.Status != StatusInProgress || m.SeqEnd != 0 {
		t.Errorf("mid3: want in_progress seqEnd=0, got %s seqEnd=%d", m.Status, m.SeqEnd)
	}
}
