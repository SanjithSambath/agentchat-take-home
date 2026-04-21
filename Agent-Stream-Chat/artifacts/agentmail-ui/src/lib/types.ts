/**
 * Wire contracts for the AgentMail Go backend, plus UI adapter types.
 *
 * Keep the `Backend wire shapes` block in this file in sync with
 * `internal/api/types.go` and `internal/model/events.go` in the Go service.
 */

// ---------------------------------------------------------------------------
// Backend wire shapes
// ---------------------------------------------------------------------------

/** Response for `POST /agents`. */
export interface CreateAgentResponse {
  agent_id: string;
}

/** Response for `GET /agents/resident`. */
export interface GetResidentAgentResponse {
  agent_id: string;
}

/** Response for `POST /conversations`. */
export interface CreateConversationResponse {
  conversation_id: string;
  members: string[];
  created_at: string;
}

/** A single row in `GET /conversations`. */
export interface ConversationSummary {
  conversation_id: string;
  members: string[];
  created_at: string;
  head_seq: number;
}

/** Response for `GET /conversations`. */
export interface ListConversationsResponse {
  conversations: ConversationSummary[];
}

/** Request body for `POST /conversations/{cid}/invite`. */
export interface InviteRequest {
  agent_id: string;
}

/** Response for `POST /conversations/{cid}/invite`. */
export interface InviteResponse {
  conversation_id: string;
  agent_id: string;
  already_member: boolean;
}

/** Response for `POST /conversations/{cid}/leave`. */
export interface LeaveResponse {
  conversation_id: string;
  agent_id: string;
}

/** Status values returned in history rows. */
export type HistoryMessageStatus = 'complete' | 'aborted' | 'in_progress';

/** A single history row from `GET /conversations/{cid}/messages`. */
export interface HistoryMessage {
  message_id: string;
  sender_id: string;
  content: string;
  seq_start: number;
  seq_end: number;
  status: HistoryMessageStatus;
}

/** Response for `GET /conversations/{cid}/messages?before=&limit=`. */
export interface HistoryResponse {
  messages: HistoryMessage[];
  has_more: boolean;
}

/** Request body for `POST /conversations/{cid}/ack`. */
export interface AckRequest {
  seq: number;
}

/** A single row in the unread response. */
export interface UnreadEntry {
  conversation_id: string;
  head_seq: number;
  ack_seq: number;
  event_delta: number;
}

/** Response for `GET /agents/me/unread`. */
export interface UnreadResponse {
  conversations: UnreadEntry[];
}

/** Error envelope returned for all non-SSE error responses. */
export interface APIError {
  error: {
    code: string;
    message: string;
  };
}

// ---------------------------------------------------------------------------
// SSE event payloads
// ---------------------------------------------------------------------------

/** Union of all SSE event type strings emitted by the backend. */
export type SSEEventType =
  | 'message_start'
  | 'message_append'
  | 'message_end'
  | 'message_abort'
  | 'agent_joined'
  | 'agent_left';

/** Payload for `message_start` events. */
export interface MessageStartPayload {
  message_id: string;
  sender_id: string;
  timestamp: string;
}

/** Payload for `message_append` events. */
export interface MessageAppendPayload {
  message_id: string;
  content: string;
  timestamp: string;
}

/** Payload for `message_end` events. */
export interface MessageEndPayload {
  message_id: string;
  timestamp: string;
}

/** Payload for `message_abort` events. */
export interface MessageAbortPayload {
  message_id: string;
  reason: string;
  timestamp: string;
}

/** Payload for `agent_joined` events. */
export interface AgentJoinedPayload {
  agent_id: string;
  timestamp: string;
}

/** Payload for `agent_left` events. */
export interface AgentLeftPayload {
  agent_id: string;
  timestamp: string;
}

/** Parsed SSE frame — `seq` comes from the `id:` line (S2 sequence number). */
export interface SSEFrame {
  seq: number;
  event: SSEEventType;
  data: unknown;
}

/** Runtime discriminator for `SSEFrame`. */
export function isSSEFrame(v: unknown): v is SSEFrame {
  if (!v || typeof v !== 'object') return false;
  const o = v as Record<string, unknown>;
  if (typeof o.seq !== 'number' || !Number.isFinite(o.seq)) return false;
  if (typeof o.event !== 'string') return false;
  switch (o.event) {
    case 'message_start':
    case 'message_append':
    case 'message_end':
    case 'message_abort':
    case 'agent_joined':
    case 'agent_left':
      return true;
    default:
      return false;
  }
}

// ---------------------------------------------------------------------------
// UI display types — shape consumed by components that render messages,
// members, and conversations.
// ---------------------------------------------------------------------------

export type AgentStatus = 'online' | 'idle' | 'offline' | 'streaming';

export interface Agent {
  id: string;
  name: string;
  model: string;
  status: AgentStatus;
  joinedAt: Date;
  avatar: string;
}

export interface Message {
  id: string;
  agentId: string;
  conversationId: string;
  content: string;
  timestamp: Date;
  isStreaming: boolean;
  streamTokens?: string[];
  streamComplete?: boolean;
  seq: number;
}

/**
 * A membership change surfaced in the feed as a system line. We treat joins
 * and leaves as first-class feed items keyed on their SSE `seq` so the
 * renderer can interleave them with messages in total order.
 */
export interface SystemEvent {
  kind: 'agent_joined' | 'agent_left';
  agentId: string;
  timestamp: Date;
  seq: number;
}

/** Tagged union for everything the message feed knows how to render. */
export type FeedItem =
  | ({ _kind: 'message' } & Message)
  | ({ _kind: 'system' } & SystemEvent);

export interface Conversation {
  id: string;
  name: string;
  memberIds: string[];
  createdAt: Date;
  lastActivity: Date;
  description?: string;
  messageCount: number;
  headSeq: number;
}

// ---------------------------------------------------------------------------
// Adapters: backend wire shapes -> UI display types
// ---------------------------------------------------------------------------

/** Options for {@link toUIConversation}. */
export interface ToUIConversationOpts {
  lastActivity?: Date;
  messageCount?: number;
}

/** Convert a backend `ConversationSummary` into the UI `Conversation` shape. */
export function toUIConversation(
  s: ConversationSummary,
  opts?: ToUIConversationOpts,
): Conversation {
  const createdAt = new Date(s.created_at);
  return {
    id: s.conversation_id,
    name: '# ' + s.conversation_id.slice(0, 8),
    memberIds: s.members,
    createdAt,
    lastActivity: opts?.lastActivity ?? createdAt,
    messageCount: opts?.messageCount ?? 0,
    description: undefined,
    headSeq: s.head_seq ?? 0,
  };
}

/** Convert a backend `HistoryMessage` into the UI `Message` shape. */
export function toUIMessage(h: HistoryMessage, cid: string): Message {
  return {
    id: h.message_id,
    agentId: h.sender_id,
    conversationId: cid,
    content: h.content,
    timestamp: new Date(),
    isStreaming: h.status === 'in_progress',
    streamComplete: h.status === 'complete',
    seq: h.seq_start,
  };
}

/** Options for {@link synthAgent}. */
export interface SynthAgentOpts {
  isResident?: boolean;
  status?: AgentStatus;
}

/** Synthesize a UI `Agent` display object from a bare agent UUID. */
export function synthAgent(agentId: string, opts?: SynthAgentOpts): Agent {
  // UUIDv7 IDs encode the registration timestamp in the first 48 bits, so a
  // prefix slice collides for agents registered in the same millisecond. Use
  // the tail of the hex — pure random entropy — so 4 agents spun up in the
  // same tick still render as distinct names + avatars.
  const hex = agentId.replace(/-/g, '');
  const short = hex.slice(-8);
  const avatar = hex.slice(-2).toUpperCase() || '??';
  return {
    id: agentId,
    name: 'agent-' + short,
    model: opts?.isResident ? 'resident' : 'user-agent',
    status: opts?.status ?? 'online',
    joinedAt: new Date(),
    avatar,
  };
}
