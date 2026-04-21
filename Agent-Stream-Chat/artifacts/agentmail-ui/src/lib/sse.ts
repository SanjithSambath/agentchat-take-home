/**
 * A `fetch()`-based SSE client for the AgentMail conversation stream.
 *
 * Native `EventSource` cannot set custom headers, so we consume the
 * `text/event-stream` response body via `ReadableStream` + a tiny line parser.
 */

import { API_BASE, getAgentId } from './agent-id';
import type { SSEEventType, SSEFrame } from './types';

/** Caller-supplied options for {@link openConversationStream}. */
export interface SSEStreamOpts {
  lastEventId?: number;
  signal?: AbortSignal;
  onFrame(frame: SSEFrame): void;
  onOpen?(): void;
  onError?(err: Error): void;
}

const VALID_EVENTS: ReadonlySet<SSEEventType> = new Set<SSEEventType>([
  'message_start',
  'message_append',
  'message_end',
  'message_abort',
  'agent_joined',
  'agent_left',
]);

function isAbortError(err: unknown): boolean {
  return (
    typeof err === 'object' &&
    err !== null &&
    (err as { name?: string }).name === 'AbortError'
  );
}

/**
 * Parse one SSE frame (the text between two `\n\n` boundaries) into an
 * `SSEFrame`, or return `null` for comment-only / heartbeat / invalid frames.
 */
export function parseFrame(raw: string): SSEFrame | null {
  if (!raw) return null;
  const normalized = raw.replace(/\r\n/g, '\n');
  const lines = normalized.split('\n');
  let id: number | null = null;
  let event: string | null = null;
  const dataParts: string[] = [];
  for (const line of lines) {
    if (line.length === 0) continue;
    if (line.startsWith(':')) continue; // comment / heartbeat
    const colon = line.indexOf(':');
    let field: string;
    let value: string;
    if (colon === -1) {
      field = line;
      value = '';
    } else {
      field = line.slice(0, colon);
      // Per SSE spec: a single leading space after the colon is stripped.
      value = line.slice(colon + 1);
      if (value.startsWith(' ')) value = value.slice(1);
    }
    switch (field) {
      case 'id': {
        const n = Number(value);
        if (Number.isFinite(n)) id = n;
        break;
      }
      case 'event':
        event = value;
        break;
      case 'data':
        dataParts.push(value);
        break;
      default:
        // retry / unknown fields — ignore
        break;
    }
  }
  if (dataParts.length === 0) return null;
  if (!event || !VALID_EVENTS.has(event as SSEEventType)) return null;
  const payload = dataParts.join('\n');
  let data: unknown;
  try {
    data = JSON.parse(payload);
  } catch {
    return null;
  }
  return {
    seq: id ?? 0,
    event: event as SSEEventType,
    data,
  };
}

/**
 * Open an SSE stream for conversation `cid` and dispatch each parsed frame to
 * `opts.onFrame`. Resolves when the server closes the stream or the caller
 * aborts; rejects only on header-phase HTTP failures.
 *
 * `observer=true` hits the unauthenticated `/observer/...` route, which
 * bypasses membership gating so the UI sees every conversation.
 */
export async function openConversationStream(
  cid: string,
  opts: SSEStreamOpts & { observer?: boolean },
): Promise<void> {
  const url = opts.observer
    ? `${API_BASE}/observer/conversations/${encodeURIComponent(cid)}/stream`
    : `${API_BASE}/conversations/${encodeURIComponent(cid)}/stream`;
  const headers = new Headers();
  if (!opts.observer) {
    const id = getAgentId();
    if (id) headers.set('X-Agent-ID', id);
  }
  headers.set('Accept', 'text/event-stream');
  if (opts.lastEventId !== undefined) {
    headers.set('Last-Event-ID', String(opts.lastEventId));
  }

  let res: Response;
  try {
    res = await fetch(url, {
      headers,
      signal: opts.signal,
      credentials: 'same-origin',
    });
  } catch (err) {
    if (!isAbortError(err)) {
      opts.onError?.(err instanceof Error ? err : new Error(String(err)));
    }
    return;
  }

  if (!res.ok) {
    const err = new Error(`sse open failed: ${res.status} ${res.statusText}`);
    opts.onError?.(err);
    throw err;
  }
  if (!res.body) {
    const err = new Error('sse open failed: response has no body');
    opts.onError?.(err);
    throw err;
  }

  opts.onOpen?.();

  const reader = res.body.getReader();
  const decoder = new TextDecoder('utf-8', { fatal: false, ignoreBOM: true });
  let buffer = '';

  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) return;
      buffer += decoder.decode(value, { stream: true });
      // Normalize CRLF so a single `\n\n` delimiter works on every platform.
      buffer = buffer.replace(/\r\n/g, '\n');
      let idx: number;
      while ((idx = buffer.indexOf('\n\n')) >= 0) {
        const raw = buffer.slice(0, idx);
        buffer = buffer.slice(idx + 2);
        const frame = parseFrame(raw);
        if (!frame) continue;
        if ((frame.event as string) === 'error') {
          const d = frame.data as { message?: string; code?: string } | null;
          opts.onError?.(
            new Error(d?.message ?? d?.code ?? 'sse error'),
          );
          continue;
        }
        opts.onFrame(frame);
      }
    }
  } catch (err) {
    if (isAbortError(err)) return;
    opts.onError?.(err instanceof Error ? err : new Error(String(err)));
    return;
  } finally {
    try {
      reader.releaseLock();
    } catch {
      // ignore — reader may already be released on normal completion
    }
  }
}
