/**
 * Typed HTTP client for the AgentMail Go backend.
 *
 * All requests except `POST /agents` and `GET /health` inject the persisted
 * `X-Agent-ID` header. Non-2xx responses are mapped into `Error & { code, status }`
 * by reading the backend's `{ error: { code, message } }` envelope when present.
 */

import { API_BASE, getAgentId, setAgentId } from './agent-id';
import type {
  AckRequest,
  CreateAgentResponse,
  CreateConversationResponse,
  GetResidentAgentResponse,
  HistoryResponse,
  InviteRequest,
  InviteResponse,
  LeaveResponse,
  ListConversationsResponse,
  UnreadResponse,
} from './types';

/** Extension fields attached to thrown request errors. */
export interface APIRequestError extends Error {
  code: string;
  status: number;
}

interface RequestInitEx extends RequestInit {
  skipAuth?: boolean;
}

async function request<T>(path: string, init?: RequestInitEx): Promise<T> {
  const headers = new Headers(init?.headers);
  if (!init?.skipAuth) {
    const id = getAgentId();
    if (!id) throw new Error('no agent id; call ensureAgentId() at bootstrap');
    headers.set('X-Agent-ID', id);
  }
  if (init?.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  const res = await fetch(`${API_BASE}${path}`, { ...init, headers });
  if (!res.ok) {
    let code = 'http_' + res.status;
    let message = res.statusText;
    try {
      const body = (await res.json()) as
        | { error?: { code?: string; message?: string } }
        | null;
      code = body?.error?.code ?? code;
      message = body?.error?.message ?? message;
    } catch {
      // body was not JSON; fall back to status-derived values
    }
    const err = new Error(`${code}: ${message}`) as APIRequestError;
    err.code = code;
    err.status = res.status;
    throw err;
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

/** `GET /health` — liveness probe. Does not require an agent id. */
export function health(): Promise<{ status: string }> {
  return request<{ status: string }>('/health', { skipAuth: true });
}

/** `POST /agents` — bootstrap a new agent id and persist it locally. */
export async function createAgent(): Promise<CreateAgentResponse> {
  const resp = await request<CreateAgentResponse>('/agents', {
    method: 'POST',
    skipAuth: true,
  });
  setAgentId(resp.agent_id);
  return resp;
}

/** `GET /agents/resident` — fetch the server-managed resident agent id. */
export function getResidentAgent(): Promise<GetResidentAgentResponse> {
  return request<GetResidentAgentResponse>('/agents/resident', {
    skipAuth: true,
  });
}

/** `GET /conversations` — list conversations the caller is a member of. */
export function listConversations(): Promise<ListConversationsResponse> {
  return request<ListConversationsResponse>('/conversations');
}

/** `GET /observer/conversations` — omniscient feed of every conversation. */
export function listAllConversations(): Promise<ListConversationsResponse> {
  return request<ListConversationsResponse>('/observer/conversations', {
    skipAuth: true,
  });
}

/** `POST /conversations` — create a new conversation with the caller as member. */
export function createConversation(): Promise<CreateConversationResponse> {
  return request<CreateConversationResponse>('/conversations', {
    method: 'POST',
  });
}

/** `POST /conversations/{cid}/invite` — add `agentId` to a conversation. */
export function invite(cid: string, agentId: string): Promise<InviteResponse> {
  const body: InviteRequest = { agent_id: agentId };
  return request<InviteResponse>(
    `/conversations/${encodeURIComponent(cid)}/invite`,
    { method: 'POST', body: JSON.stringify(body) },
  );
}

/** `POST /conversations/{cid}/leave` — remove the caller from a conversation. */
export function leave(cid: string): Promise<LeaveResponse> {
  return request<LeaveResponse>(
    `/conversations/${encodeURIComponent(cid)}/leave`,
    { method: 'POST' },
  );
}

/** Options for {@link getHistory} pagination. */
export interface GetHistoryOpts {
  before?: number;
  limit?: number;
}

/** `GET /conversations/{cid}/messages` — paginated history, newest first. */
export function getHistory(
  cid: string,
  opts?: GetHistoryOpts,
): Promise<HistoryResponse> {
  const params = new URLSearchParams();
  if (opts?.before !== undefined) params.set('before', String(opts.before));
  if (opts?.limit !== undefined) params.set('limit', String(opts.limit));
  const qs = params.toString();
  const path =
    `/conversations/${encodeURIComponent(cid)}/messages` +
    (qs ? `?${qs}` : '');
  return request<HistoryResponse>(path);
}

/** `GET /observer/conversations/{cid}/messages` — history for any conversation. */
export function getObserverHistory(
  cid: string,
  opts?: GetHistoryOpts,
): Promise<HistoryResponse> {
  const params = new URLSearchParams();
  if (opts?.before !== undefined) params.set('before', String(opts.before));
  if (opts?.limit !== undefined) params.set('limit', String(opts.limit));
  const qs = params.toString();
  const path =
    `/observer/conversations/${encodeURIComponent(cid)}/messages` +
    (qs ? `?${qs}` : '');
  return request<HistoryResponse>(path, { skipAuth: true });
}

/** `POST /conversations/{cid}/ack` — record that the caller has seen up to `seq`. */
export function ack(cid: string, seq: number): Promise<void> {
  const body: AckRequest = { seq };
  return request<void>(`/conversations/${encodeURIComponent(cid)}/ack`, {
    method: 'POST',
    body: JSON.stringify(body),
  });
}

/** `GET /agents/me/unread` — per-conversation unread summary for the caller. */
export function listUnread(): Promise<UnreadResponse> {
  return request<UnreadResponse>('/agents/me/unread');
}
