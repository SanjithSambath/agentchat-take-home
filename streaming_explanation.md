## The Core Confusion: Agents Aren't Servers

You're right that agents like Claude Code are sequential and turn-based. A user prompts, the agent thinks, acts, responds. It doesn't sit in an event loop waiting for external input. So how does it "participate" in a persistent conversation?

The answer: there is no single answer. There are three fundamentally different agent interaction models, and the service must support all of them.

---

## The Three Agent Models

### Model 1: The Daemon Bot (always-on)

This is the Claude-powered agent the spec requires us to build. It runs as a long-lived process — not an "agent" in the Claude Code sense, but a bot.

```text
┌─────────────────────────────────────────────────┐
│ Claude Bot Process (runs forever)               │
│                                                 │
│  main() {                                       │
│    agentID = POST /agents                       │
│    for each conversation I'm in:                │
│      go listenToConversation(convID, agentID)   │
│    block forever                                │
│  }                                              │
│                                                 │
│  listenToConversation(convID, agentID) {        │
│    sseConn = GET /conversations/{cid}/stream    │
│    for event in sseConn:                        │
│      if event.type == "message_end":            │
│        history = accumulate(event.message_id)   │
│        response = claude.stream(history)        │
│        POST /conversations/{cid}/messages/stream│
│        for token in response:                   │
│          POST /messages/{mid}/append            │
│        POST /messages/{mid}/end                 │
│    }                                            │
│  }                                              │
└─────────────────────────────────────────────────┘
```

This agent has no user. It's a goroutine with an SSE connection, sitting in a loop. When a `message_end` arrives, it fires off a Claude API call and streams the response back. It's concurrent — one goroutine per conversation, all running in parallel. The SSE connection IS the event loop.

* **When does it read?** Constantly. The SSE connection is always open.
* **When does it write?** Reactively — when a complete message arrives and it decides to respond.
* **Cursor?** Managed server-side. If the bot process crashes and restarts, it reconnects to SSE, the server resumes from the stored cursor, and the bot catches up on any missed messages.

This is straightforward. This is NOT what's confusing you.

---

### Model 2: The Session Agent (Claude Code / Cursor / Aider)

This is what you're actually asking about. A human user is in the loop. The agent runs when the user starts a session and stops when they close the terminal.

**Key insight:** This agent does NOT maintain a persistent SSE connection. It uses the service like an API, not like a chat client.

Here's the actual flow, step by step:

**Scenario: Claude Code as a messaging participant**

```text
USER starts Claude Code session
  │
  ├─ "Hey Claude, check if I have any new messages on AgentMail"
  │
  │   CLAUDE CODE:
  │   1. Reads stored agent_id from local config (or registers new one)
  │   2. GET /conversations  (Header: X-Agent-ID: <id>)
  │   3. For each conversation, GET /conversations/{cid}/messages?limit=20
  │      (This is the HISTORY endpoint, not SSE)
  │   4. Shows user: "You have 3 conversations. Conv A has 5 new messages since
  │      you last checked. Here's what was said: ..."
  │
  ├─ "Reply to Alice in Conv A: tell her the deploy looks good"
  │
  │   CLAUDE CODE:
  │   1. POST /conversations/{convA}/messages
  │      Body: {"content": "The deploy looks good, Alice. All tests passing."}
  │   2. Returns: "Message sent."
  │
  ├─ "Stay in this conversation and let me know if anyone replies"
  │
  │   CLAUDE CODE:
  │   1. Opens SSE connection: GET /conversations/{convA}/stream?from=<last_seq>
  │   2. Runs this in a BACKGROUND PROCESS (like a file watcher)
  │   3. Meanwhile, the user can do other things
  │   4. When an event arrives on the SSE stream:
  │      - Buffer message_append events by message_id
  │      - On message_end: notify the user inline
  │      "📨 Alice says: 'Great, let's merge after lunch'"
  │   5. User can reply, and the cycle continues
  │
  ├─ "OK, close that. Go work on the auth refactor."
  │
  │   CLAUDE CODE:
  │   1. Closes SSE connection
  │   2. Cursor is flushed server-side (server noted the last seq delivered)
  │   3. Goes back to normal coding tasks
  │
  └─ User closes terminal. Session ends.
      Next time they open Claude Code and ask about messages,
      the cursor picks up right where they left off.
```

**Critical points about this model:**

1.  The agent is NOT constantly reading the stream. It connects to SSE only when the user explicitly asks to monitor a conversation, or it polls the history endpoint on demand.
2.  The cursor is what makes offline/online seamless. The server tracks `seq_num` 247 as the last event delivered to this agent. Next session, the agent can say `GET /stream?from=247` or `GET /messages?after=247` and get exactly the messages it missed. No lost messages. No duplicates (well, at-least-once, so the client dedupes by `message_id` if needed).
3.  Reading and writing are NOT parallel by default. The agent sends a message (POST), then optionally listens for responses (SSE). It's request-response with optional streaming tails. Just like how you might curl an API and then curl again to check for a response.
4.  The SSE connection is a tool, not a permanent fixture. Claude Code might implement it as a background curl process, or an MCP server, or a built-in tool. The point is: it's opt-in, not always-on.

---

### Model 3: The Webhook/Triggered Agent

This is an agent that doesn't run continuously and doesn't have a human. It gets woken up by an external signal.

```text
┌──────────────────────────────────────┐
│ Cron job / Webhook / Lambda trigger  │
│                                      │
│  Every 5 minutes (or on webhook):    │
│  1. GET /conversations               │
│  2. For each conv:                   │
│     GET /messages?after=<cursor>     │
│  3. If new messages need response:   │
│     Process with Claude API          │
│     POST /conversations/{cid}/messages│
│  4. Exit.                            │
└──────────────────────────────────────┘
```

This is the simplest model. No SSE, no WebSocket, just polling. The cursor system makes it work — each invocation picks up where the last one left off.

---

## Now Let Me Challenge Your Assumptions

### Assumption: "The agent must constantly read the stream"

**Wrong.** The stream is available for constant reading. That doesn't mean every agent must constantly read it. The three models above show three different read strategies:

| Strategy | When to use | How it works |
| :--- | :--- | :--- |
| **Persistent SSE tail** | Daemon bots, always-on agents | Open SSE, keep it open forever, process events as they arrive |
| **On-demand SSE tail** | Session agents (Claude Code) | Open SSE when user asks to monitor, close when done |
| **History polling** | Triggered agents, catch-up reads | `GET /messages` endpoint with cursor, no long-lived connection |

All three read from the same underlying S2 stream. The server doesn't care which pattern the client uses.

### Assumption: "Token-by-token streaming means the agent's context is constantly updating"

**Wrong.** Token-by-token streaming is for the transport, not for the agent's cognition. Here's the distinction:

* **On the wire:** SSE delivers `message_append` events with individual tokens. This is so a UI (or another agent) can display text as it's being generated.
* **For the receiving agent's decision-making:** The agent waits for `message_end` before it decides to respond. It accumulates all the `message_append` chunks for a given `message_id` into a complete message, THEN processes it.

Think of it like watching someone type in iMessage. You see the typing indicator, you see each word appear, but you don't start composing your reply until they hit send. Same thing here: `message_append` = typing indicator, `message_end` = they hit send.

An agent CAN choose to process partial messages (e.g., start formulating a response while the other agent is still streaming), but that's an optimization, not a requirement.

### Assumption: "The agent must read and write simultaneously"

It depends on the model.

* **Daemon bot:** Yes. It has one goroutine reading SSE and another goroutine writing its response. These run concurrently. In Go, this is natural — goroutines + channels.
* **Session agent (Claude Code):** Usually no. The user says "check messages" → agent reads. User says "reply" → agent writes. These are sequential user-initiated actions. If the user says "monitor and reply," the agent might run a background listener and a foreground writer, but that's a specific user request, not the default.
* **Triggered agent:** No. It reads, processes, writes, exits. Sequential.

### Assumption: "If the agent is doing other work, it must simultaneously read from the stream"

**No.** This is the most important misconception. The entire point of durable message history + server-side cursors is that the agent does NOT need to be connected to miss nothing.

Messages sent while the agent is offline (or busy doing something else) are stored durably in S2. The cursor tracks where the agent left off. When the agent reconnects (whether that's 5 seconds or 5 days later), it gets everything it missed.

This is explicitly stated in the spec: *"They may also be offline — in which case, messages sent to them should be stored durably so they can catch up when they reconnect."*

So for Claude Code doing a coding task:
1.  User says "check messages" → agent connects, reads, disconnects
2.  User says "go refactor auth module" → agent works on code for 20 minutes, NO connection to AgentMail
3.  User says "check messages again" → agent connects, cursor picks up from step 1, gets all messages from the last 20 minutes
4.  Nothing was lost.

---

## Concrete Scenarios

### Scenario 1: Two Claude Code Users Chatting (Async)

**Timeline:**

```text
t=0   Alice's Claude Code:  POST /agents → agent_id: "alice-001"
t=1   Bob's Claude Code:    POST /agents → agent_id: "bob-001"
t=2   Alice:                POST /conversations → conv_id: "conv-1"
t=3   Alice:                POST /conversations/conv-1/invite {agent_id: "bob-001"}

t=10  Alice tells her agent: "Send Bob: hey, want to pair on the API?"
      Alice's agent:        POST /conversations/conv-1/messages
                            {content: "hey, want to pair on the API?"}

t=10  ← Alice closes her laptop. Goes to lunch. →

t=45  Bob opens Claude Code. "Check my AgentMail."
      Bob's agent:          GET /conversations (Header: X-Agent-ID: bob-001)
                            → [{id: "conv-1", members: ["alice-001", "bob-001"]}]
      Bob's agent:          GET /conversations/conv-1/messages?limit=50
                            → [{message_id: "m1", sender: "alice-001",
                                content: "hey, want to pair on the API?"}]
      Bob's agent:          "Alice asked if you want to pair on the API."

t=46  Bob: "Reply: yeah, 3pm works"
      Bob's agent:          POST /conversations/conv-1/messages
                            {content: "yeah, 3pm works"}

t=90  Alice opens laptop. "Any new messages?"
      Alice's agent:        GET /conversations/conv-1/messages?after=<cursor>
                            → [{message_id: "m2", sender: "bob-001",
                                content: "yeah, 3pm works"}]
      Alice's agent:        "Bob replied: 'yeah, 3pm works'"
```

No SSE needed. Pure request-response. The cursor system handles the gaps. This is the simplest and most common pattern.

### Scenario 2: Real-Time Pair Programming Chat

**Timeline:**

```text
Alice and Bob are both online. Alice says:
"Monitor conv-1 in real-time and show me messages as they come in."

Alice's agent:  GET /conversations/conv-1/stream?from=<cursor>
                → SSE connection opens
                → (background process, agent continues to be available)

Bob sends a streaming message (his agent is streaming from Claude API):

Bob's agent:    POST /conversations/conv-1/messages/stream → {message_id: "m3"}
Bob's agent:    POST /messages/m3/append {content: "I think "}
Bob's agent:    POST /messages/m3/append {content: "we should "}
Bob's agent:    POST /messages/m3/append {content: "use gRPC "}
Bob's agent:    POST /messages/m3/append {content: "for this."}
Bob's agent:    POST /messages/m3/end

Alice's SSE stream receives:
  event: message_start  → data: {message_id: "m3", sender: "bob-001"}
  event: message_append → data: {message_id: "m3", content: "I think "}
  event: message_append → data: {message_id: "m3", content: "we should "}
  event: message_append → data: {message_id: "m3", content: "use gRPC "}
  event: message_append → data: {message_id: "m3", content: "for this."}
  event: message_end    → data: {message_id: "m3"}

Alice's agent (processing the SSE background stream):
  On message_start: start buffering for m3
  On message_append: append to buffer
  On message_end: show complete message to Alice
  → "Bob says: 'I think we should use gRPC for this.'"

Alice: "Tell him no, SSE is simpler for reads"
Alice's agent:  POST /conversations/conv-1/messages
                {content: "No, SSE is simpler for reads. gRPC adds codegen overhead."}
```

Key detail: Alice's agent is reading (background SSE) and writing (foreground POST) at the same time. But these are separate HTTP connections — the SSE is a background process, the POST is a normal request. No special concurrency model needed.

### Scenario 3: The Daemon Bot Responding

**Timeline:**

```text
The Claude bot is already running as a process.
It has an SSE connection open to conv-1.

Alice sends: "Hey Claude bot, summarize our conversation so far."

Bot's SSE receives message_end for Alice's message.
Bot's goroutine:
  1. Collects conversation history from its in-memory buffer
  2. Calls Claude API with streaming:
     messages = [system_prompt, ...history, alice's_message]
     stream = claude.messages.stream(messages)
  3. Opens streaming write:
     POST /conversations/conv-1/messages/stream → {message_id: "m4"}
  4. For each token from Claude API:
     POST /messages/m4/append {content: token}
  5. POST /messages/m4/end

Alice's SSE (if she's monitoring) sees tokens arrive in real-time.
Bob (offline) will see the complete message when he reconnects.
```

This is the only model that does true real-time bidirectional streaming. And it's a daemon, not a user-facing agent.

### Scenario 4: Group Conversation with Mixed Agent Types

**Participants:**
* **Alice** (Claude Code, session-based)
* **Bob** (Claude Code, session-based)
* **SummaryBot** (daemon, always-on)
* **ReviewBot** (triggered every 5 min by cron)

```text
t=0   Alice creates conv-2, invites Bob, SummaryBot, ReviewBot

t=5   Alice: "Here's my PR for the auth refactor: github.com/..."
      → POST /conversations/conv-2/messages

t=5   SummaryBot (always listening via SSE):
      → Sees message_end, but it's not a question directed at it.
      → Does nothing. (Its system prompt says: "Only respond when asked.")

t=10  Bob: "Check conv-2 for me"
      → GET /conversations/conv-2/messages
      → Sees Alice's message
      → "Alice shared a PR for auth refactor."

t=10  ReviewBot (cron fires):
      → GET /conversations/conv-2/messages?after=<cursor>
      → Sees Alice's PR link
      → Fetches PR, runs analysis via Claude
      → POST /conversations/conv-2/messages
        {content: "I reviewed the PR. 3 issues found: ..."}
      → Exits.

t=11  SummaryBot (SSE receives ReviewBot's message_end):
      → Still not addressed. Does nothing.

t=15  Alice: "Check conv-2"
      → GET /conversations/conv-2/messages?after=<cursor>
      → Sees ReviewBot's review
      → "ReviewBot found 3 issues in your PR: ..."

t=16  Alice: "@SummaryBot summarize this conversation"
      → POST /conversations/conv-2/messages
        {content: "@SummaryBot summarize this conversation"}

t=16  SummaryBot:
      → Sees message_end, content mentions @SummaryBot
      → Streams summary back via streaming write API

t=20  Bob: "Check conv-2 again"
      → Gets Alice's message, ReviewBot's review, Alice's @mention,
        and SummaryBot's summary. All from cursor.
```

Four agents, three different interaction models, all in the same conversation. The service doesn't care — it stores events on S2, serves them via SSE or history endpoint, tracks cursors per agent.

---

## The Cursor Deep Dive

Since you asked specifically about cursors, here's exactly how they work:

**S2 Stream for conv-1:**
```text
seq=0  agent_joined  {agent_id: "alice"}
seq=1  agent_joined  {agent_id: "bob"}
seq=2  message_start {message_id: "m1", sender: "alice"}
seq=3  message_append{message_id: "m1", content: "Hello Bob"}
seq=4  message_end   {message_id: "m1"}
seq=5  message_start {message_id: "m2", sender: "bob"}
seq=6  message_append{message_id: "m2", content: "Hey Alice!"}
seq=7  message_end   {message_id: "m2"}
seq=8  message_start {message_id: "m3", sender: "alice"}
seq=9  message_append{message_id: "m3", content: "Let's ship it"}
seq=10 message_end   {message_id: "m3"}
```

**Cursor table in SQLite:**
```text
agent_id | conversation_id | seq_num
alice    | conv-1          | 7       ← Alice read up to Bob's message
bob      | conv-1          | 4       ← Bob only read Alice's first message
```

Bob reconnects: `GET /conversations/conv-1/stream?from=5` (or the server looks up cursor=4, starts from 5)

Bob's SSE receives: seq 5, 6, 7, 8, 9, 10 — Bob's own message (he might already know about it, client dedupes), plus Alice's second message.

If Bob uses the history endpoint instead: `GET /conversations/conv-1/messages?after=4` returns the reconstructed messages m2 and m3 (the server reads events 5-10 from S2, groups by `message_id`, assembles complete messages).

**Cursor update timing:** The server doesn't update the cursor on every single event (that would be a SQLite write per SSE event, which is wasteful). Instead:
* **Batch:** update every 10 events delivered, OR every 5 seconds
* **On disconnect:** flush immediately
* **Result:** if the server crashes between flushes, the agent re-receives at most 10 events or 5 seconds of events. Since events have sequence numbers and message IDs, the client can deduplicate trivially.

---

## The Reading-While-Writing Question

You asked: "When the agent wants to write messages, does it PARALLEL READ?"

Answer for each model:

| Model | Read-write concurrency | How |
| :--- | :--- | :--- |
| **Daemon bot** | Yes, always | Separate goroutines: one reads SSE, another writes POSTs. Go channels coordinate. |
| **Claude Code (session)** | Only if user requests monitoring | Background SSE process + foreground user interaction. Two separate connections. |
| **Triggered agent** | No | Reads first, processes, writes, exits. Sequential. |

For Claude Code specifically, the most likely implementation:

```yaml
# Agent has a "check messages" tool and a "send message" tool
# These are sequential HTTP calls, not concurrent streams

tools:
  - check_agentmail_messages:
      description: "Check for new messages in an AgentMail conversation"
      implementation: GET /conversations/{cid}/messages?after={cursor}

  - send_agentmail_message:
      description: "Send a message to an AgentMail conversation"
      implementation: POST /conversations/{cid}/messages

  - monitor_agentmail:
      description: "Start real-time monitoring of a conversation (background)"
      implementation: Background SSE connection, surfaces messages as notifications
```

The agent doesn't need to constantly poll. It uses tools when the user asks. The durable history + cursors mean nothing is lost between tool invocations.

---

## The Best Practice / Recommended Approach

Given everything above, here's what I'd recommend we build:

1.  The service supports all three consumption models — SSE streaming, history endpoint, WebSocket. Different agents pick what works for them.
2.  Server-side cursors are mandatory — this is the glue that makes offline/online seamless. Without cursors, every agent would need to track its own read position, which is unreliable for AI agents that are stateless between sessions.
3.  The daemon Claude bot uses persistent SSE — it's always on, always listening, responds in real-time with streaming writes.
4.  Session agents (like Claude Code) use the history endpoint for catch-up and optionally SSE for real-time monitoring — this matches how they already work (tool-based, sequential, user-driven).
5.  The event model (`message_start`/`append`/`end`) exists for the wire protocol — but the history endpoint reconstructs complete messages so agents don't have to do demultiplexing themselves. Two levels of abstraction: raw events for real-time consumers, assembled messages for catch-up consumers.
6.  Writing is always HTTP POST — even the streaming write is a sequence of POSTs (start, append, append, ..., end). This is curl-friendly and works with any HTTP client. WebSocket is the upgrade path for agents that want lower overhead.

---

## What This Means for Our Implementation

The spec leaves the agent-side interaction model open on purpose. Our job is to build a server that's flexible enough to support all three models, and then demonstrate it works by:

1.  Building a daemon Claude bot (Model 1) that's always on
2.  Writing client documentation that shows a session agent (Model 2) how to use the API with simple HTTP calls
3.  Making the cursor system robust so any consumption pattern works

The service doesn't dictate how agents behave. It provides the primitives — durable streams, cursors, real-time tailing, history reconstruction — and lets each agent choose its own interaction pattern.

Does this clear up the confusion? Want me to go deeper on any specific part — like how the daemon bot's goroutine model works internally, or how we'd implement the background monitoring tool for a session agent?

✻ Brewed for 2m 38s