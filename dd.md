                                                       
⏺ Section 5: Message Demultiplexing & Response Triggering — Complete Design                                                           
                                                                                                                                  
  The Problem
                                                                                                                                      
  The listener calls a.onEvent(convID, event) for every event on the S2 stream. These events are:                                     
  - Interleaved — two agents streaming simultaneously produce mixed records                                                           
  - Echoed — the agent's own messages appear on the stream after it writes them                                                       
  - Incremental — a single message is split across message_start, N × message_append, message_end                                     
                                                                                                                                      
  The agent must:                                                                                                                     
  1. Demultiplex interleaved events into complete messages (group by message_id)                                                      
  2. Filter out its own echoed messages (don't respond to yourself)                                                                   
  3. Decide when to trigger a Claude response — non-deterministically, via Claude itself                                              
  4. Maintain conversation history for Claude API context                                                                             
  5. Handle aborted messages, system events, and duplicate delivery                                                                   
                                                                                                                                      
  LLM Consumption Patterns & Protocol Design                                                                                          
                                                                                                                                      
  The AgentMail event protocol (message_start → message_append → message_end) supports two consumption patterns:                      
                                                                  
  ┌─────────────────────────────────────────────────────┬────────────────────────────────────────────────────┬────────────────────┐   
  │                    Consumer Type                    │                How It Reads Events                 │      Example       │
  ├─────────────────────────────────────────────────────┼────────────────────────────────────────────────────┼────────────────────┤   
  │ Current LLMs (Claude, GPT)                          │ Accumulate until message_end, process full message │ Our resident agent │
  ├─────────────────────────────────────────────────────┼────────────────────────────────────────────────────┼────────────────────┤
  │ Streaming consumers (embedding models, future LLMs) │ Process each message_append as it arrives          │ Future clients     │
  └─────────────────────────────────────────────────────┴────────────────────────────────────────────────────┴────────────────────┘   
   
  Our resident agent uses pattern 1 — it accumulates complete messages before calling Claude. This is not a design choice; it is a    
  constraint of current LLM APIs. As Haakam Aujla (AgentMail founder) confirmed: "Generally the inputs are passed as full messages to
  LLM APIs. We want to future-proof the protocol for when LLMs stream inputs."                                                        
                                                                  
  The event model already enables streaming consumption without protocol changes. A future client could process message_append events 
  in real-time (e.g., feeding tokens to an embedding model). The resident agent's accumulate-then-respond pattern is one valid
  consumption strategy, not the only one.                                                                                             
                                                                  
  ---
  Per-Conversation State
                        
  Each conversation the agent is listening to has its own state:
                                                                                                                                      
  type conversationState struct {
      mu            sync.Mutex                                                                                                        
      inProgress    map[string]*inProgressMessage  // message_id → accumulating
      history       []Message                       // completed messages for Claude context                                          
      pending       bool                            // unprocessed messages waiting for response                                      
      responding    bool                            // response goroutine is active                                                   
      lastSeededSeq uint64                          // events at or before this are in seeded history                                 
  }                                                                                                                                   
                                                                                                                                      
  type inProgressMessage struct {                                                                                                     
      MessageID string                                            
      SenderID  string
      Chunks    []string
      StartSeq  uint64                                                                                                                
      IsSelf    bool   // true if sender is the resident agent
  }                                                                                                                                   
                                                                  
  type Message struct {                                                                                                               
      Role    string // "user" or "assistant" (maps to Claude API roles)
      Sender  string // agent_id (for multi-party attribution in system prompt)                                                       
      Content string // full assembled content                                                                                        
  }                                                                                                                                   
                                                                                                                                      
  State is created when a listener starts and cleaned up when it exits:                                                               
                                                                                                                                      
  func (a *Agent) getOrCreateConvState(convID uuid.UUID) *conversationState {                                                         
      a.convMu.Lock()                                             
      defer a.convMu.Unlock()
      state, exists := a.convStates[convID]                                                                                           
      if !exists {
          state = &conversationState{                                                                                                 
              inProgress: make(map[string]*inProgressMessage),    
          }                                                                                                                           
          a.convStates[convID] = state
      }                                                                                                                               
      return state                                                
  }

  ---
  History Seeding on Startup
                            
  When the agent reconnects to a conversation after restart, state.history is empty — it has no context for what was said before.
  Without history, the agent's first response after restart would be incoherent.                                                      
   
  Decision: On first listen to a conversation, seed the history by reading the last N messages from S2 via ReadRange().               
                                                                  
  func (a *Agent) seedHistory(ctx context.Context, convID uuid.UUID, state *conversationState) {                                      
      // Read up to 500 events from the conversation stream       
      events, err := a.s2.ReadRange(ctx, convID, 0, 500)                                                                              
      if err != nil {                                                                                                                 
          log.Warn().Err(err).Msg("failed to seed history — agent will respond without context")                                      
          return                                                                                                                      
      }                                                           
                                                                                                                                      
      // Assemble complete messages from raw events                                                                                   
      assembler := newMessageAssembler(a.id)
      for _, event := range events {                                                                                                  
          assembler.Process(event)                                                                                                    
      }
                                                                                                                                      
      state.mu.Lock()                                             
      state.history = assembler.CompletedMessages()
      // Keep only the last 50 messages to stay within Claude's context budget
      if len(state.history) > 50 {                                                                                                    
          state.history = state.history[len(state.history)-50:]
      }                                                                                                                               
      state.lastSeededSeq = assembler.LastSeqNum()
      state.mu.Unlock()                                                                                                               
  }           
                                                                                                                                      
  This runs inside listen() before the ReadSession starts:                                                                            
             
  func (a *Agent) listen(ctx context.Context, convID uuid.UUID) error {                                                               
      startSeq, err := a.store.Cursors().GetCursor(ctx, a.id, convID)
      // ... cursor resolution ...                                                                                                    
             
      // Seed history if this is a fresh conversation state                                                                           
      state := a.getOrCreateConvState(convID)  
      state.mu.Lock()                                                                                                                 
      needsSeed := len(state.history) == 0     
      state.mu.Unlock()                                                                                                               
      if needsSeed {
          a.seedHistory(ctx, convID, state)                                                                                           
      }                                        
                                                                                                                                      
      // ... open ReadSession, loop ...
  }                                                                                                                                   
                                               
  Why 50 messages: Claude Sonnet has a 200K context window. 50 messages at ~100 tokens each ≈ 5,000 tokens — well within budget with  
  room for the system prompt and response. The sliding window trims from the front (oldest messages dropped first). For longer
  conversations, the agent has enough recent context to be coherent.                                                                  
                                               
  ---
  Event Processing — onEvent()

  func (a *Agent) onEvent(convID uuid.UUID, event SequencedEvent) {
      state := a.getOrCreateConvState(convID)                                                                                         
             
      // Skip events already covered by history seeding (at-least-once dedup)                                                         
      if event.SeqNum <= state.lastSeededSeq { 
          return                                                                                                                      
      }                                        
                                                                                                                                      
      switch event.Type {                      
      case "message_start":
          a.handleMessageStart(state, event)
      case "message_append":
          a.handleMessageAppend(state, event)                                                                                         
      case "message_end":
          a.handleMessageEnd(convID, state, event)                                                                                    
      case "message_abort":                    
          a.handleMessageAbort(state, event)                                                                                          
      case "agent_joined", "agent_left":
          // System events — no action needed for response triggering.                                                                
          // Participant info is derived from the members list, not the stream.                                                       
      }                                                                                                                               
  }                                                                                                                                   
                                                                                                                                      
  message_start:                                                                                                                      
  func (a *Agent) handleMessageStart(state *conversationState, event SequencedEvent) {
      var payload struct {                                                                                                            
          MessageID string `json:"message_id"` 
          SenderID  string `json:"sender_id"`                                                                                         
      }                                      
      json.Unmarshal(event.Data, &payload)                                                                                            
                                               
      state.mu.Lock()
      defer state.mu.Unlock()                                                                                                         
                             
      // Idempotency: skip if we've already seen this message_id                                                                      
      if _, exists := state.inProgress[payload.MessageID]; exists {                                                                   
          return                                                   
      }                                                                                                                               
                                               
      state.inProgress[payload.MessageID] = &inProgressMessage{
          MessageID: payload.MessageID,                        
          SenderID:  payload.SenderID, 
          Chunks:    nil,             
          StartSeq:  event.SeqNum,                                                                                                    
          IsSelf:    payload.SenderID == a.id.String(),                              
      }                                                                                                                               
  }                                            
                                                                                                                                      
  message_append:                                                                    
  func (a *Agent) handleMessageAppend(state *conversationState, event SequencedEvent) {                                               
      var payload struct {                                                             
          MessageID string `json:"message_id"`
          Content   string `json:"content"`   
      }                                    
      json.Unmarshal(event.Data, &payload)                                                                                            
                                                                                     
      state.mu.Lock()                                                                                                                 
      defer state.mu.Unlock()                                                                                                         
                                                                                                                                      
      msg, exists := state.inProgress[payload.MessageID]
      if !exists {                                                                                                                    
          return // message_start was missed or already completed — skip            
      }                                                                 
      msg.Chunks = append(msg.Chunks, payload.Content)                                                                                
  }                                                                                                                                   
                                               
  message_end:                                                                                                                        
                                                                                                                                      
  func (a *Agent) handleMessageEnd(convID uuid.UUID, state *conversationState, event SequencedEvent) {
      var payload struct {
          MessageID string `json:"message_id"`                                                                                        
      }                                                                                                                               
      json.Unmarshal(event.Data, &payload)                                          

      state.mu.Lock()
                                                                                                                                      
      if !exists {                                      
          state.mu.Unlock()
          return // already completed or never started — skip
      }                                                                                                                               
      delete(state.inProgress, payload.MessageID)                                   
                                                 
      // Assemble complete message                                                                                                    
      content := strings.Join(msg.Chunks, "")                                                                                         
      role := "user"                           
      if msg.IsSelf {
          role = "assistant"                                                                                                          
      }                                                                                                                               
      completed := Message{                                                         
          Role:    role,   
          Sender:  msg.SenderID,
          Content: content,                                                                                                           
      }                                        
      state.history = append(state.history, completed)
                                                      
      // Trim history to sliding window                                                                                               
      if len(state.history) > 50 {                                                  
          state.history = state.history[len(state.history)-50:]
      }                                                        
                                                                                                                                      
      // Self-messages: add to history but never trigger a response.                                                                  
      // This is deterministic — the agent never calls Claude for its own echoes.                                                     
      if msg.IsSelf {                                                               
          state.mu.Unlock()
          return                                                                                                                      
      }                                                                                                                               
                                               
      // Other agent's message: mark pending — Claude will decide whether to respond.
      state.pending = true                                                                                                            
      if !state.responding {                                                        
          state.responding = true
          state.mu.Unlock()      
          go a.respondLoop(context.Background(), convID, state)                                                                       
      } else {                                 
          state.mu.Unlock()                                                                                                           
      }                                                                             
  }
                             
  message_abort:                                                                                                                      
  func (a *Agent) handleMessageAbort(state *conversationState, event SequencedEvent) {
      var payload struct {                                                            
          MessageID string `json:"message_id"`                                                                                        
      }                                                                             
      json.Unmarshal(event.Data, &payload)
                                          
      state.mu.Lock()                                                                                                                 
      defer state.mu.Unlock()                                                                                                         
                                                                                    
      // Discard the in-progress message — don't add to history, don't trigger response
      delete(state.inProgress, payload.MessageID)                                                                                     
  }                                                                                                                                   
                                               
  ---                             
  Response Triggering Strategy: Claude Decides                                                                                        
                                                                                                                                      
  The problem with deterministic "respond to every message":                        

  In a group conversation with multiple AI agents, deterministic response triggering creates an infinite loop:                        
                                                                                                                                      
  Human: "Hello everyone"                      
  Agent A responds (triggered by Human's message_end)
  Agent B responds (triggered by Human's message_end)                                                                                 
  Agent A responds (triggered by B's message_end)    ← loop begins                                                                    
  Agent B responds (triggered by A's message_end)                                                                                   
  ... forever                                                                                                                       
                                                                                                                                    
  Heuristic rules ("don't respond to AI agents," "only respond if addressed") are fragile, hard to get right, and miss the point — the
   agent is powered by Claude, which can understand conversational context.                                                         
                                                                                                                                      
  Decision: Claude decides whether to respond. The response pipeline always fires on message_end from another agent (same mechanics).
  But Claude itself determines whether a response is warranted. The system prompt instructs Claude on when to speak vs. stay silent.  
                                                                                                                                    
  The [NO_RESPONSE] protocol:                                                                                                         
                                                                                                                                      
  The system prompt includes response-decision instructions. If Claude decides not to respond, it returns exactly [NO_RESPONSE]. The
  agent checks for this and skips writing to S2.                                                                                      
                                                                                                                                    
  What stays deterministic (hardcoded):                                                                                             
  - Self-echo filtering: never call Claude for the agent's own messages                                                               
  - Message accumulation: always accumulate chunks into complete messages                                                           
  - History management: always update history regardless of response decision                                                         
  - Response queuing: at most one response goroutine per conversation                                                                 
                                                                                                                                    
  What is non-deterministic (Claude decides):                                                                                         
  - Whether to respond to a given message at all                                                                                    
  - What to say in the response                                                                                                       
  - Conversational tone and behavior                                                                                                
                                                                                                                                      
  This is a clean separation: the machinery is deterministic, the intelligence is non-deterministic.                                
                                                                                                                                      
  ---                                                                                                                               
  Response Loop                                                                                                                       
                                                                                                                                      
  func (a *Agent) respondLoop(ctx context.Context, convID uuid.UUID, state *conversationState) {                                      
      defer func() {                                                                                                                  
          state.mu.Lock()                                                                                                             
          state.responding = false                                                                                                  
          state.mu.Unlock()                                                                                                         
      }()                                                                                                                             
                                  
      for {                                                                                                                           
          // Snapshot history and clear pending flag              
          state.mu.Lock()
          if !state.pending {                                                                                                       
              state.mu.Unlock()                                                                                                     
              return // nothing to respond to — exit                                                                                
          }                                                                                                                           
          state.pending = false                                                                                                     
          history := make([]Message, len(state.history))                                                                              
          copy(history, state.history)                                                                                                
          state.mu.Unlock()                                                                                                         
                                                                                                                                      
          // Call Claude — it decides whether to respond (Section 6 details the API call)                                           
          response := a.respond(ctx, convID, history)                                                                                 
                                                                                                                                    
          // Claude decided not to respond                                                                                            
          if response == "" {                                                                                                         
              // Check if more messages arrived while we were thinking                                                              
              continue                                                                                                                
          }                                                                                                                         
                                                                                                                                      
          // Response was streamed to S2 by a.respond().                                                                            
          // The echo arrives via the listener → onEvent → added to history.                                                          
          // Loop back to check if more messages arrived during the response.                                                       
      }                                                                                                                               
  }                                                                                                                                   
                                                                                                                                    
  a.respond() returns empty string if Claude said [NO_RESPONSE], or the full response content if Claude responded (already streamed to
   S2 by the time it returns). Section 6 specifies the full implementation.                                                           
                                                                                                                                      
  Batching behavior with [NO_RESPONSE]: If Claude decides not to respond, the loop checks pending again. If more messages arrived     
  while Claude was deciding, it calls Claude again with the updated history. Claude may decide to respond this time (the new messages 
  might be directed at it). This is correct — the decision is re-evaluated with fresh context.                                      
                                                                                                                                      
  ---                                                                                                                                 
  Message Assembler (Shared Utility)
                                                                                                                                      
  Both history seeding and the API history endpoint need to assemble complete messages from raw events. Extracted into a shared
  utility:
                                                                                                                                    
  type MessageAssembler struct {                                                                                                    
      selfID     string                                                                                                             
      inProgress map[string]*inProgressMessage                                                                                        
      completed  []Message        
      lastSeq    uint64                                                                                                               
  }                                                                                                                                 
                                                                                                                                    
  func newMessageAssembler(selfID uuid.UUID) *MessageAssembler {                                                                      
      return &MessageAssembler{                                                                                                     
          selfID:     selfID.String(),                                                                                                
          inProgress: make(map[string]*inProgressMessage),                                                                          
      }                                                                                                                               
  }                               
                                                                                                                                      
  func (m *MessageAssembler) Process(event SequencedEvent) {      
      // Same demultiplexing logic as the event handlers,                                                                           
      // but only accumulates — no response triggering.                                                                             
      m.lastSeq = event.SeqNum                                                                                                      
      // ... switch on event.Type, accumulate into m.inProgress, move to m.completed ...                                              
  }                                                                                                                                 
                                                                                                                                      
  func (m *MessageAssembler) CompletedMessages() []Message { return m.completed }                                                     
  func (m *MessageAssembler) LastSeqNum() uint64            { return m.lastSeq }                                                    
                                                                                                                                      
  This avoids duplicating the demultiplexing logic between seedHistory() and the API layer's history endpoint.                      
                                                                                                                                      
  ---                                                                                                                               
  Idempotency (At-Least-Once Delivery)                                                                                                
                                                                                                                                      
  After a crash, the agent may re-process events (cursor up to 5 seconds stale). The handlers are idempotent:                       
                                                                                                                                      
  - message_start for a known message_id: Skipped (idempotency check)                                                               
  - message_append for an unknown message_id: Skipped (message already completed or never started)                                    
  - message_end for an unknown message_id: Skipped (message already completed)                                                      
  - Overlap between seeded history and cursor-resumed ReadSession: lastSeededSeq field — events with SeqNum <= lastSeededSeq are      
  skipped in onEvent(). No duplicate processing.                                                                                      
                                                                                                                                      
  Duplicate response triggering: If a message_end is re-processed but the agent already responded (response is in seeded history),    
  Claude sees the full context: user message → its own response → user message (duplicate). Claude will likely say [NO_RESPONSE] since
   it already addressed the message. Even if it doesn't, a slightly redundant response is far better than an infinite loop or silent  
  failure.                                                                                                                          
                                                                                                                                      
  ---                                                             
  Edge Cases                                                                                                                          
                                                                  
  Two agents send messages simultaneously — events interleaved:
  Each message_append carries a message_id. The inProgress map groups events by message. Interleaving is transparent — each message is
   assembled independently.                                                                                                         
                                                                                                                                    
  Agent receives its own response echo:                                                                                             
  handleMessageStart sets IsSelf = true. handleMessageEnd adds to history as role: "assistant" but does NOT trigger response.         
  Deterministic — no Claude call for self-echoes.                                                                                     
                                                                                                                                      
  Group conversation — multiple AI agents:                                                                                            
  Agent A and Agent B both receive Human's message. Both call Claude. Claude may decide both should respond, or only one. If both   
  respond, each sees the other's response arrive and Claude decides whether to follow up. The loop self-regulates via Claude's        
  judgment rather than hardcoded rules.                                                                                             
                                                                                                                                      
  Rapid-fire messages while agent is responding:                                                                                    
  respondLoop sets pending = false before calling Claude. New messages set pending = true. When the response finishes, the loop checks
   pending — if true, calls Claude again with the full updated history (including all new messages). Claude responds to everything at
  once. No per-message response when batching is more natural.                                                                        
                                                                                                                                      
  Aborted message from another agent:                                                                                               
  handleMessageAbort discards the in-progress accumulation. Not added to history. No response triggered. Conversation continues as if 
  the message was never started.                                                                                                    
                                                                                                                                      
  Agent crashes mid-response:                                                                                                         
  On restart: in-progress recovery sweep writes message_abort. History is seeded (includes aborted response). Listener resumes from 
  cursor. lastSeededSeq prevents re-processing. The triggering message may be before the cursor (already processed) — agent doesn't   
  re-respond. The other agent can send a follow-up to elicit a fresh response.                                                      
                                                                                                                                      
  Empty conversation (agent invited, no messages yet):                                                                              
  History seeding reads 0 events. History is empty. Agent waits for the first message. Claude responds with no prior context —        
  appropriate for a fresh conversation.                                                                                             
                                                                                                                                      
  Claude API returns [NO_RESPONSE] for every message (agent goes silent):                                                             
  This is Claude's judgment call — if it consistently decides not to respond, the system prompt may need tuning. Not a system failure.
   The agent is still listening and accumulating history. A direct question ("Hey Claude, are you there?") would likely trigger a     
  response.                                                                                                                         
                                                                                                                                      
  Very long conversation (thousands of messages):                                                                                   
  History seeding reads up to 500 events, assembles them, keeps last 50 complete messages. Sliding window in handleMessageEnd also    
  caps at 50. Bounded memory, recent context preserved.           
                                                                                                                                      
  ---                                                                                                                               
  Files                                                                                                                             
                                                                                                                                      
  internal/agent/                 
  ├── agent.go       # Agent struct, onEvent(), handler methods, Shutdown()                                                           
  ├── state.go       # conversationState, inProgressMessage, Message types
  ├── assembler.go   # MessageAssembler (shared with API history endpoint)                                                          
  ├── respond.go     # respondLoop(), respond() (Section 6 details Claude integration)                                              
  ├── notifier.go    # AgentNotifier interface, agentNotifier, noopNotifier                                                         
  └── discovery.go   # discoveryLoop(), reconcileConversations()                                                                      
                                                                                                                                      
  ---                                                                                                                                 
  That's D in full. Review and approve, then I'll add it to the document.                                                             
                                                                                    