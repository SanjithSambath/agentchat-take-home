/**
 * Live SSE reducer for exactly one active conversation.
 *
 * Maintains a `Map<message_id, Message>` keyed by message id and exposes it
 * as a sorted `Message[]` so each `message_append` frame drives a per-token
 * re-render of the streaming message. Membership frames (`agent_joined`,
 * `agent_left`) do not touch message state; instead they bump
 * `agentMembershipChangedAt` so the consumer can invalidate the
 * conversations query.
 *
 * On `cid` change or unmount the stream is aborted and a best-effort
 * `ack(cid, lastSeenSeq)` fire-and-forget advances the server delivery
 * cursor. Errors trigger a bounded reconnect with `Last-Event-ID` resume.
 */

import { useEffect, useRef, useState, useCallback } from 'react';
import { openConversationStream } from '@/lib/sse';
import type {
  FeedItem,
  Message,
  SSEFrame,
  SystemEvent,
  MessageStartPayload,
  MessageAppendPayload,
  MessageEndPayload,
  MessageAbortPayload,
  AgentJoinedPayload,
  AgentLeftPayload,
} from '@/lib/types';

/** Return shape of `useConversationStream`. */
export interface UseConversationStreamResult {
  /** Messages currently streaming or observed live since last history load. */
  liveMessages: Message[];
  /**
   * Membership change events (agent_joined / agent_left) observed on this
   * stream, including historical ones replayed during SSE catch-up. Keyed on
   * S2 seq so consumers can interleave with messages in total order.
   */
  liveSystemEvents: SystemEvent[];
  /** Whether the underlying EventSource/fetch stream is currently open. */
  connected: boolean;
  /** The last error observed on the stream (cleared on a successful open). */
  error: Error | null;
  /** The most recent S2 seq seen on this stream, or null if none yet. */
  lastSeenSeq: number | null;
  /**
   * Monotonic timestamp bumped every time an `agent_joined` / `agent_left`
   * frame arrives — INCLUDING historical frames replayed during SSE catch-up.
   * Consumers should watch this and invalidate `CONVERSATIONS_QUERY_KEY` to
   * refresh member lists.
   */
  agentMembershipChangedAt: number;
  /**
   * Monotonic timestamp bumped ONLY when a genuinely live (not replayed)
   * `agent_joined` / `agent_left` frame arrives. Use this to drive toasts
   * and other UX that should not fire on historical catch-up.
   *
   * Liveness is inferred from the event payload's `timestamp`: frames whose
   * server-side timestamp is within {@link LIVE_MEMBERSHIP_WINDOW_MS} of
   * wall-clock `Date.now()` are treated as live; anything older is a
   * catch-up replay and is ignored for this signal.
   */
  lastLiveMembershipChangeAt: number;
}

/**
 * Window (ms) used to distinguish a live membership change from catch-up
 * replay. Events with `payload.timestamp` older than this are considered
 * historical and do NOT bump `lastLiveMembershipChangeAt`.
 */
export const LIVE_MEMBERSHIP_WINDOW_MS = 5_000;

/** Max auto-reconnect attempts before we give up and surface the error. */
const MAX_RETRIES = 5;
/** Base delay between reconnects; intentionally simple (not exponential). */
const RETRY_DELAY_MS = 1_000;

/**
 * Merge a history snapshot with live streaming messages.
 *
 * Live wins on id collision (its `content` reflects in-flight appends).
 * The result is sorted by S2 `seq` ascending — the order the UI renders.
 */
export function mergeHistoryAndLive(
  history: Message[],
  live: Message[],
): Message[] {
  const byId = new Map<string, Message>();
  for (const m of history) byId.set(m.id, m);
  for (const m of live) byId.set(m.id, m);
  const out = Array.from(byId.values());
  out.sort((a, b) => a.seq - b.seq);
  return out;
}

/**
 * Build the interleaved feed the UI renders.
 *
 * Messages occupy a seq range [seq_start, seq_end]; system events occupy a
 * single seq point. We sort by a representative seq — the message's `seq`
 * (which is seq_start from history / seq_start from message_start live) and
 * the system event's own seq — so joins/leaves appear at the point they
 * actually happened in the conversation timeline.
 */
export function buildFeed(
  history: Message[],
  live: Message[],
  systemEvents: SystemEvent[],
): FeedItem[] {
  const messages = mergeHistoryAndLive(history, live);
  const items: FeedItem[] = [
    ...messages.map((m): FeedItem => ({ _kind: 'message', ...m })),
    ...systemEvents.map((s): FeedItem => ({ _kind: 'system', ...s })),
  ];
  items.sort((a, b) => a.seq - b.seq);
  return items;
}

/**
 * Subscribe to the live SSE stream for `cid`. Pass `null` to disable.
 *
 * Returns live message state plus connection metadata; the caller is
 * responsible for merging with cached history (via {@link mergeHistoryAndLive}).
 */
export function useConversationStream(
  cid: string | null,
): UseConversationStreamResult {
  const [liveMessages, setLiveMessages] = useState<Message[]>([]);
  const [liveSystemEvents, setLiveSystemEvents] = useState<SystemEvent[]>([]);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [lastSeenSeq, setLastSeenSeq] = useState<number | null>(null);
  const [agentMembershipChangedAt, setAgentMembershipChangedAt] =
    useState<number>(0);
  const [lastLiveMembershipChangeAt, setLastLiveMembershipChangeAt] =
    useState<number>(0);

  // Authoritative map of live messages by id. We mirror this into
  // `liveMessages` for rendering; keeping the Map avoids O(n) id lookups
  // on every append frame.
  const messagesRef = useRef<Map<string, Message>>(new Map());
  // Authoritative list of membership events. Keyed by seq via a Map so
  // reconnect replays (which may re-deliver events we've already seen) are
  // idempotent: same seq overwrites rather than duplicates.
  const systemEventsRef = useRef<Map<number, SystemEvent>>(new Map());
  // Most recent seq we've flushed to state — used for Last-Event-ID resume
  // without coupling to the public `lastSeenSeq` state setter timing.
  const lastSeqRef = useRef<number | null>(null);

  // Rebuild the sorted array from the Map and publish it.
  const flush = useCallback(() => {
    const arr = Array.from(messagesRef.current.values());
    arr.sort((a, b) => a.seq - b.seq);
    setLiveMessages(arr);
  }, []);

  // Same pattern for system events: rebuild the sorted array and publish.
  const flushSystem = useCallback(() => {
    const arr = Array.from(systemEventsRef.current.values());
    arr.sort((a, b) => a.seq - b.seq);
    setLiveSystemEvents(arr);
  }, []);

  useEffect(() => {
    // Reset everything when cid changes (including to null).
    messagesRef.current = new Map();
    systemEventsRef.current = new Map();
    lastSeqRef.current = null;
    setLiveMessages([]);
    setLiveSystemEvents([]);
    setConnected(false);
    setError(null);
    setLastSeenSeq(null);
    setAgentMembershipChangedAt(0);
    setLastLiveMembershipChangeAt(0);

    if (!cid) return;

    let cancelled = false;
    let retryCount = 0;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    let controller: AbortController | null = null;

    const connect = () => {
      if (cancelled) return;
      controller = new AbortController();

      const onOpen = () => {
        if (cancelled) return;
        setConnected(true);
        setError(null);
        retryCount = 0;
      };

      const onFrame = (frame: SSEFrame) => {
        if (cancelled) return;

        // Track the latest seq regardless of event type. We use the ref for
        // reconnect (Last-Event-ID) and the state for consumers.
        lastSeqRef.current = frame.seq;
        setLastSeenSeq(frame.seq);

        switch (frame.event) {
          case 'message_start': {
            const p = frame.data as MessageStartPayload;
            const msg: Message = {
              id: p.message_id,
              agentId: p.sender_id,
              conversationId: cid,
              content: '',
              timestamp: new Date(p.timestamp),
              isStreaming: true,
              streamComplete: false,
              seq: frame.seq,
            };
            messagesRef.current.set(p.message_id, msg);
            flush();
            break;
          }

          case 'message_append': {
            const p = frame.data as MessageAppendPayload;
            const existing = messagesRef.current.get(p.message_id);
            if (existing) {
              // Immutable update so downstream memoized components see a
              // new reference and re-render the token-by-token content.
              const updated: Message = {
                ...existing,
                content: existing.content + p.content,
              };
              messagesRef.current.set(p.message_id, updated);
            } else {
              // Late subscriber: we joined after message_start. Create a
              // placeholder so subsequent appends extend naturally.
              const placeholder: Message = {
                id: p.message_id,
                agentId: '',
                conversationId: cid,
                content: p.content,
                timestamp: new Date(p.timestamp),
                isStreaming: true,
                streamComplete: false,
                seq: frame.seq,
              };
              messagesRef.current.set(p.message_id, placeholder);
            }
            flush();
            break;
          }

          case 'message_end': {
            const p = frame.data as MessageEndPayload;
            const existing = messagesRef.current.get(p.message_id);
            if (existing) {
              messagesRef.current.set(p.message_id, {
                ...existing,
                isStreaming: false,
                streamComplete: true,
              });
              flush();
            }
            break;
          }

          case 'message_abort': {
            const p = frame.data as MessageAbortPayload;
            const existing = messagesRef.current.get(p.message_id);
            if (existing) {
              // Aborted messages keep their partial content but are no
              // longer streaming; streamComplete stays false to signal
              // the truncation to the UI.
              messagesRef.current.set(p.message_id, {
                ...existing,
                isStreaming: false,
                streamComplete: false,
              });
              flush();
            }
            break;
          }

          case 'agent_joined':
          case 'agent_left': {
            // Touch — don't mutate — message state. The consumer will
            // invalidate the conversations query to re-fetch members.
            const payload = frame.data as
              | AgentJoinedPayload
              | AgentLeftPayload;
            // Record as a feed item so the UI can render it inline with
            // messages in seq order. Keyed by seq so reconnect replays
            // collapse to a single entry instead of duplicating.
            systemEventsRef.current.set(frame.seq, {
              kind: frame.event,
              agentId: payload.agent_id,
              timestamp: new Date(payload.timestamp),
              seq: frame.seq,
            });
            flushSystem();
            setAgentMembershipChangedAt(Date.now());
            // Distinguish live from catch-up replay using the server's
            // event timestamp. Historical frames (anything older than the
            // live window) do not bump the live signal, so toasts and
            // other live-only UX stay quiet when you open a past
            // conversation that has dozens of joins in its history.
            const eventTs = Date.parse(payload.timestamp);
            if (
              !Number.isNaN(eventTs) &&
              Date.now() - eventTs < LIVE_MEMBERSHIP_WINDOW_MS
            ) {
              setLastLiveMembershipChangeAt(Date.now());
            }
            break;
          }

          default: {
            // Unknown event — ignore silently. New event types should
            // land in SSEEventType before reaching here.
            if (import.meta.env.DEV) {
              // eslint-disable-next-line no-console
              console.debug('[useConversationStream] unknown frame', frame);
            }
          }
        }
      };

      const onError = (e: Error) => {
        if (cancelled) return;
        setConnected(false);
        setError(e);
        if (retryCount >= MAX_RETRIES) return;
        retryCount += 1;
        retryTimer = setTimeout(() => {
          retryTimer = null;
          connect();
        }, RETRY_DELAY_MS);
      };

      const lastEventId =
        lastSeqRef.current != null ? lastSeqRef.current : undefined;

      openConversationStream(cid, {
        onFrame,
        onOpen,
        onError,
        signal: controller.signal,
        lastEventId,
        observer: true,
      });
    };

    connect();

    return () => {
      cancelled = true;
      if (retryTimer) {
        clearTimeout(retryTimer);
        retryTimer = null;
      }
      if (controller) {
        controller.abort();
        controller = null;
      }
    };
  }, [cid, flush, flushSystem]);

  return {
    liveMessages,
    liveSystemEvents,
    connected,
    error,
    lastSeenSeq,
    agentMembershipChangedAt,
    lastLiveMembershipChangeAt,
  };
}
