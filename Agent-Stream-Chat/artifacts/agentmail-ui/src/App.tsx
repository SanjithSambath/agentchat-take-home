import { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ConversationSidebar } from '@/components/ConversationSidebar';
import { ConversationHeader } from '@/components/ConversationHeader';
import { MessageFeed } from '@/components/MessageFeed';
import { MemberList } from '@/components/MemberList';
import { useConversations, CONVERSATIONS_QUERY_KEY } from '@/hooks/useConversations';
import { useMessages, MESSAGES_QUERY_KEY } from '@/hooks/useMessages';
import { useConversationStream, mergeHistoryAndLive, buildFeed } from '@/hooks/useConversationStream';
import { getAgentId } from '@/lib/agent-id';
import { getResidentAgent } from '@/lib/api';
import { synthAgent, type Agent, type Conversation } from '@/lib/types';

/** A conv whose head_seq advanced within this window is "actively streaming". */
const ACTIVE_WINDOW_MS = 4_000;

export default function App() {
  const qc = useQueryClient();
  const [activeConvId, setActiveConvId] = useState<string | null>(null);
  const [toasts, setToasts] = useState<{ id: number; text: string }[]>([]);

  const convQ = useConversations();
  const conversations = convQ.data ?? [];

  // Per-conv head_seq tracking: when did each conv's head_seq last advance,
  // and what was the seq value the user last viewed. Live entirely in refs +
  // a rerender tick so we don't loop on the polling response.
  const lastSeenSeqRef = useRef<Map<string, number>>(new Map());
  const lastAdvanceAtRef = useRef<Map<string, number>>(new Map());
  const viewedSeqRef = useRef<Map<string, number>>(new Map());
  const [activityTick, setActivityTick] = useState(0);

  // Detect head_seq advances from each poll response.
  useEffect(() => {
    if (!convQ.data) return;
    const now = Date.now();
    let advanced = false;
    for (const c of convQ.data) {
      const prev = lastSeenSeqRef.current.get(c.id) ?? -1;
      if (c.headSeq > prev) {
        lastSeenSeqRef.current.set(c.id, c.headSeq);
        lastAdvanceAtRef.current.set(c.id, now);
        advanced = true;
        // First time we see a conv, treat its current head as already-viewed
        // so it doesn't immediately flag as unread.
        if (prev === -1 && !viewedSeqRef.current.has(c.id)) {
          viewedSeqRef.current.set(c.id, c.headSeq);
        }
      }
    }
    if (advanced) setActivityTick((t) => t + 1);
  }, [convQ.data]);

  // Tick once a second so the "active" badge decays without needing a poll.
  useEffect(() => {
    const id = setInterval(() => setActivityTick((t) => t + 1), 1_000);
    return () => clearInterval(id);
  }, []);

  // Mark active conv as viewed up to its current head whenever it changes
  // OR new content streams in while we're looking at it.
  useEffect(() => {
    if (!activeConvId) return;
    const current = lastSeenSeqRef.current.get(activeConvId);
    if (current !== undefined) {
      viewedSeqRef.current.set(activeConvId, current);
      setActivityTick((t) => t + 1);
    }
  }, [activeConvId, convQ.data]);

  const now = Date.now();
  const activeIds = new Set<string>();
  const unreadIds = new Set<string>();
  for (const c of conversations) {
    const last = lastAdvanceAtRef.current.get(c.id) ?? 0;
    if (now - last < ACTIVE_WINDOW_MS) activeIds.add(c.id);
    const viewed = viewedSeqRef.current.get(c.id) ?? 0;
    if (c.id !== activeConvId && c.headSeq > viewed) unreadIds.add(c.id);
  }
  void activityTick;

  // Auto-select the first conversation when the list loads. If empty, we
  // fall through to the EmptyState — this UI is read-only, so conversations
  // are created externally by agents connecting into the resident.
  useEffect(() => {
    if (!convQ.isSuccess) return;
    if (activeConvId) return;
    if (conversations.length > 0) {
      setActiveConvId(conversations[0].id);
    }
  }, [convQ.isSuccess, conversations, activeConvId]);

  const msgQ = useMessages(activeConvId);
  const stream = useConversationStream(activeConvId);

  // Invalidate the conversations list on ANY membership change — including
  // historical catch-up — because the sidebar's member counts need to match
  // the server after reconnects or conversation switches.
  useEffect(() => {
    if (stream.agentMembershipChangedAt === 0) return;
    qc.invalidateQueries({ queryKey: CONVERSATIONS_QUERY_KEY });
    if (activeConvId) {
      qc.invalidateQueries({ queryKey: MESSAGES_QUERY_KEY(activeConvId) });
    }
  }, [stream.agentMembershipChangedAt, qc, activeConvId]);

  // Show the toast only for genuinely live joins/leaves — never for the
  // dozens of historical `agent_joined` frames replayed when you open a
  // past conversation. The hook filters those out upstream by comparing
  // each frame's payload timestamp against wall clock.
  //
  // Dismiss timers are tracked in a ref so we can let each one fire on its
  // own schedule. The previous implementation cleared the newest timer on
  // every effect re-run, which left older toasts stuck on screen forever.
  const toastTimersRef = useRef<Map<number, ReturnType<typeof setTimeout>>>(new Map());
  useEffect(() => {
    if (stream.lastLiveMembershipChangeAt === 0) return;
    const id = stream.lastLiveMembershipChangeAt;
    setToasts(prev => [{ id, text: '[*] membership updated' }, ...prev].slice(0, 3));
    const timer = setTimeout(() => {
      setToasts(prev => prev.filter(x => x.id !== id));
      toastTimersRef.current.delete(id);
    }, 2500);
    toastTimersRef.current.set(id, timer);
  }, [stream.lastLiveMembershipChangeAt]);

  // Clear any pending dismiss timers on unmount to avoid setState-after-
  // unmount noise. We deliberately do NOT clear them on every toast effect
  // re-run — see above.
  useEffect(() => {
    return () => {
      for (const t of toastTimersRef.current.values()) clearTimeout(t);
      toastTimersRef.current.clear();
    };
  }, []);

  const messages = useMemo(
    () => mergeHistoryAndLive(msgQ.data ?? [], stream.liveMessages),
    [msgQ.data, stream.liveMessages]
  );

  const feed = useMemo(
    () => buildFeed(msgQ.data ?? [], stream.liveMessages, stream.liveSystemEvents),
    [msgQ.data, stream.liveMessages, stream.liveSystemEvents]
  );

  const activeConv: Conversation | null = useMemo(() => {
    if (!activeConvId) return null;
    const found = conversations.find(c => c.id === activeConvId);
    if (found) {
      return { ...found, messageCount: messages.length };
    }
    // Conversation just created — synthesize a stub until the list refetches.
    return {
      id: activeConvId,
      name: '# ' + activeConvId.slice(0, 8),
      memberIds: [],
      createdAt: new Date(),
      lastActivity: new Date(),
      messageCount: messages.length,
      headSeq: 0,
    };
  }, [activeConvId, conversations, messages.length]);

  // Look up the server-managed resident agent id once. This is the ONLY correct
  // signal for "is this member the resident"; the prior heuristic (memberIds[1])
  // silently mislabeled every non-resident peer as resident. 503 on deployments
  // without a resident configured is expected — we treat that as "no resident"
  // and never apply the label.
  const residentQ = useQuery({
    queryKey: ['resident-agent'],
    queryFn: async () => {
      try {
        return (await getResidentAgent()).agent_id;
      } catch {
        return null;
      }
    },
    staleTime: Infinity,
    retry: false,
  });
  const residentAgentId = residentQ.data ?? null;

  // Build the Agent[] the UI renders by synthesizing from member ids observed
  // across the active conversation + any senders present in visible messages.
  const myAgentId = getAgentId();
  const agents: Agent[] = useMemo(() => {
    const ids = new Set<string>();
    if (activeConv) activeConv.memberIds.forEach(id => ids.add(id));
    for (const m of messages) if (m.agentId) ids.add(m.agentId);
    // System events carry ids for agents who joined/left; pull them in so
    // the feed can render a name for every historical participant, not just
    // current members.
    for (const s of stream.liveSystemEvents) if (s.agentId) ids.add(s.agentId);
    return Array.from(ids).map(id =>
      synthAgent(id, {
        isResident: residentAgentId !== null && id === residentAgentId && id !== myAgentId,
        status: stream.liveMessages.some(m => m.agentId === id && m.isStreaming)
          ? 'streaming'
          : 'online',
      })
    );
  }, [activeConv, messages, stream.liveMessages, stream.liveSystemEvents, residentAgentId, myAgentId]);

  return (
    <div className="dark h-screen w-screen flex flex-col overflow-hidden bg-background text-foreground">
      <div className="flex flex-1 overflow-hidden">
        <ConversationSidebar
          conversations={conversations}
          activeId={activeConvId ?? ''}
          onSelect={setActiveConvId}
          streamingIds={activeIds}
          unreadIds={unreadIds}
        />

        <div className="flex flex-col flex-1 overflow-hidden relative">
          {toasts.length > 0 && (
            <div className="absolute top-14 left-1/2 -translate-x-1/2 z-30 pointer-events-none flex flex-col gap-1 items-center">
              {toasts.map(toast => (
                <span
                  key={toast.id}
                  className="font-mono text-[10px] text-muted-foreground bg-surface-0/85 border border-border px-2 py-1 rounded animate-pulse-subtle"
                >
                  {toast.text}
                </span>
              ))}
            </div>
          )}
          {activeConv ? (
            <>
              <ConversationHeader conversation={activeConv} agents={agents} />
              <MessageFeed
                feed={feed}
                agents={agents}
                conversationId={activeConv.id}
              />
            </>
          ) : (
            <EmptyState
              loading={convQ.isLoading}
              error={convQ.isError ? (convQ.error as Error).message : null}
            />
          )}
        </div>

        {activeConv && <MemberList conversation={activeConv} agents={agents} />}
      </div>
    </div>
  );
}

function EmptyState({ error, loading }: { error: string | null; loading: boolean }) {
  if (error) {
    return (
      <div className="flex-1 flex items-center justify-center">
        <div className="font-mono text-xs text-destructive/80">{error}</div>
      </div>
    );
  }

  if (loading) {
    return (
      <div className="flex-1 flex items-center justify-center">
        <div className="font-mono text-xs text-muted-foreground/60">loading conversations…</div>
      </div>
    );
  }

  return (
    <div className="flex-1 flex items-center justify-center relative overflow-hidden">
      {/* Passive scanner line sweeping the empty canvas */}
      <div className="absolute inset-y-0 w-40 bg-gradient-to-r from-transparent via-primary/5 to-transparent animate-scan-sweep pointer-events-none" />

      <div className="relative flex flex-col items-center gap-8 px-6">
        <div className="flex flex-col items-start gap-0.5 self-start font-mono text-[10px]">
          <span className="text-online animate-line-in" style={{ animationDelay: '0ms' }}>
            [boot] initializing resident…
          </span>
          <span className="text-muted-foreground/60 animate-line-in" style={{ animationDelay: '180ms' }}>
            [wait] listening on stream bus
          </span>
          <span className="text-idle animate-line-in" style={{ animationDelay: '360ms' }}>
            [idle] awaiting inbound agent
          </span>
        </div>
        {/* Radar: concentric pings radiating outward */}
        <div className="relative w-32 h-32 flex items-center justify-center">
          {[0, 0.9, 1.8].map(delay => (
            <div
              key={delay}
              className="absolute inset-0 rounded-full border border-primary/50 animate-radar-ping"
              style={{ animationDelay: `${delay}s` }}
            />
          ))}
          <div className="relative w-3 h-3 rounded-full bg-primary/80 shadow-[0_0_12px_hsl(var(--primary))]" />
        </div>

        {/* Status stack */}
        <div className="flex flex-col items-center gap-1.5">
          <span className="font-mono text-[10px] tracking-[0.25em] text-muted-foreground/60 uppercase">
            no conversations
          </span>
          <span className="font-mono text-sm text-foreground/90">
            scanning for agents
            <span className="animate-blink text-primary">_</span>
          </span>
          <span className="font-mono text-[10px] text-muted-foreground/50">
            waiting for an inbound stream to open
          </span>
        </div>

        {/* Resident agent ready card */}
        <div className="mt-2 flex items-center gap-3 px-4 py-2.5 rounded border border-border bg-surface-0">
          <div className="relative flex items-center justify-center">
            <div className="absolute w-3 h-3 rounded-full bg-online/40 animate-ping" />
            <div className="relative w-1.5 h-1.5 rounded-full bg-online" />
          </div>
          <div className="flex flex-col">
            <span className="font-mono text-[10px] tracking-widest uppercase text-muted-foreground/70">
              resident agent
            </span>
            <span className="font-mono text-[11px] text-online">
              online · ready to connect
            </span>
          </div>
        </div>
      </div>
    </div>
  );
}
