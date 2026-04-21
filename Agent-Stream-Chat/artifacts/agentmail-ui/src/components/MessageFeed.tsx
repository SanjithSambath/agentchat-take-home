import { useEffect, useRef, useState, useCallback } from 'react';
import type { Message, Agent, FeedItem, SystemEvent } from '@/lib/types';
import { cn } from '@/lib/utils';
import { Copy, Check } from 'lucide-react';

interface MessageFeedProps {
  feed: FeedItem[];
  agents: Agent[];
  conversationId: string;
}

/**
 * A "block" is a run of adjacent feed items that belong together. Messages
 * from the same agent back-to-back fold into a single MessageGroup; each
 * system event is its own block so join/leave lines always stand alone.
 */
type Block =
  | { kind: 'messages'; id: string; agent: Agent | undefined; messages: Message[] }
  | { kind: 'system'; id: string; event: SystemEvent };

export function MessageFeed({ feed, agents, conversationId }: MessageFeedProps) {
  const bottomRef = useRef<HTMLDivElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const [isAtBottom, setIsAtBottom] = useState(true);

  const scrollToBottom = useCallback(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, []);

  useEffect(() => {
    if (isAtBottom) scrollToBottom();
  }, [feed, isAtBottom, scrollToBottom]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'instant' });
  }, [conversationId]);

  const handleScroll = () => {
    const el = containerRef.current;
    if (!el) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    setIsAtBottom(atBottom);
  };

  const blocks = groupFeed(feed, agents);
  const firstMessage = feed.find(f => f._kind === 'message') as
    | (FeedItem & { _kind: 'message' })
    | undefined;

  return (
    <div
      ref={containerRef}
      onScroll={handleScroll}
      className="flex-1 overflow-y-auto px-4 py-4 space-y-0.5"
    >
      <div className="flex items-center gap-3 mb-6">
        <div className="flex-1 border-t border-border" />
        <span className="font-mono text-[9px] text-muted-foreground/40 uppercase tracking-widest px-2">
          stream opened · {(firstMessage?.timestamp ?? new Date()).toISOString().slice(0, 10)}
        </span>
        <div className="flex-1 border-t border-border" />
      </div>

      {blocks.map((block, bi) => {
        if (block.kind === 'system') {
          return (
            <SystemLine
              key={block.id}
              event={block.event}
              agents={agents}
            />
          );
        }
        return (
          <MessageGroup
            key={block.id}
            group={block}
            isLast={bi === blocks.length - 1}
          />
        );
      })}

      {feed.length === 0 && (
        <div className="flex items-center justify-center py-12">
          <span className="font-mono text-[10px] text-muted-foreground/40">
            no messages yet · send one to start the stream
          </span>
        </div>
      )}

      <div ref={bottomRef} className="h-2" />
    </div>
  );
}

type MsgBlock = Extract<Block, { kind: 'messages' }>;

function groupFeed(feed: FeedItem[], agents: Agent[]): Block[] {
  const blocks: Block[] = [];
  let currentMsg: MsgBlock | null = null;
  const agentById = new Map(agents.map(a => [a.id, a]));

  for (const item of feed) {
    if (item._kind === 'system') {
      currentMsg = null;
      blocks.push({
        kind: 'system',
        id: 'sys-' + item.seq,
        event: {
          kind: item.kind,
          agentId: item.agentId,
          timestamp: item.timestamp,
          seq: item.seq,
        },
      });
      continue;
    }

    const agent = agentById.get(item.agentId);
    if (!currentMsg || currentMsg.agent?.id !== item.agentId) {
      currentMsg = { kind: 'messages', id: item.id, agent, messages: [item] };
      blocks.push(currentMsg);
    } else {
      currentMsg.messages.push(item);
    }
  }

  return blocks;
}

function SystemLine({ event, agents }: { event: SystemEvent; agents: Agent[] }) {
  const agent = agents.find(a => a.id === event.agentId);
  const name = agent?.name ?? 'agent-' + event.agentId.slice(0, 8);
  const verb = event.kind === 'agent_joined' ? 'joined' : 'left';
  const color = event.kind === 'agent_joined' ? 'text-online' : 'text-muted-foreground/60';
  return (
    <div className="flex items-center gap-3 py-1 px-2 -mx-2">
      <div className="flex-1 border-t border-border/40" />
      <span className="font-mono text-[10px] tracking-wide">
        <span className="text-muted-foreground/40">[{event.seq}]</span>{' '}
        <span className={cn('font-semibold', color)}>{name}</span>{' '}
        <span className="text-muted-foreground/60">{verb} the conversation</span>{' '}
        <span className="text-muted-foreground/30">· {formatTime(event.timestamp)}</span>
      </span>
      <div className="flex-1 border-t border-border/40" />
    </div>
  );
}

function MessageGroup({ group }: { group: MsgBlock; isLast: boolean }) {
  const { agent, messages } = group;
  const firstMsg = messages[0];

  const statusColor =
    agent?.status === 'streaming' ? 'bg-streaming border-streaming/30' :
    agent?.status === 'online' ? 'bg-muted border-border' :
    agent?.status === 'idle' ? 'bg-muted border-border' :
    'bg-muted/40 border-border/50';

  const avatarTextColor =
    agent?.status === 'streaming' ? 'text-streaming' :
    agent?.status === 'offline' ? 'text-muted-foreground/30' :
    'text-muted-foreground';

  return (
    <div className="flex gap-3 hover:bg-accent/10 rounded px-2 py-1 -mx-2 group transition-colors">
      <div className="shrink-0 mt-0.5">
        <div className={cn('w-8 h-8 rounded-sm border flex items-center justify-center', statusColor)}>
          <span className={cn('font-mono text-[10px] font-bold', avatarTextColor)}>
            {agent?.avatar ?? '??'}
          </span>
        </div>
      </div>

      <div className="flex-1 min-w-0">
        <div className="flex items-baseline gap-2 mb-0.5">
          <span className={cn(
            'font-mono text-xs font-semibold',
            agent?.status === 'streaming' ? 'text-streaming' :
            agent?.status === 'offline' ? 'text-muted-foreground/50' :
            'text-foreground/90'
          )}>
            {agent?.name ?? 'unknown-agent'}
          </span>
          <span className="font-mono text-[9px] text-muted-foreground/40">
            {agent?.model}
          </span>
          <span className="font-mono text-[9px] text-muted-foreground/30 ml-auto">
            {formatTime(firstMsg.timestamp)}
          </span>
          <span className="font-mono text-[9px] text-muted-foreground/20 tabular-nums">
            seq:{firstMsg.seq}
          </span>
        </div>

        <div className="space-y-1.5">
          {messages.map(msg => (
            <MessageContent key={msg.id} message={msg} />
          ))}
        </div>
      </div>
    </div>
  );
}

function MessageContent({ message }: { message: Message }) {
  const [copied, setCopied] = useState(false);
  // Keep the phosphor caret around for ~400ms past isStreaming going false
  // so the decay feels physical instead of snapping out.
  const prevStreaming = useRef(message.isStreaming);
  const [fading, setFading] = useState(false);
  useEffect(() => {
    if (prevStreaming.current && !message.isStreaming) {
      setFading(true);
      const t = setTimeout(() => setFading(false), 400);
      prevStreaming.current = message.isStreaming;
      return () => clearTimeout(t);
    }
    prevStreaming.current = message.isStreaming;
    return undefined;
  }, [message.isStreaming]);

  const handleCopy = () => {
    navigator.clipboard.writeText(message.content);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  const lines = message.content.split('\n');
  const showCaret = message.isStreaming || fading;

  return (
    <div className="relative group/msg">
      <div className={cn(
        'font-mono text-xs leading-relaxed whitespace-pre-wrap',
        message.isStreaming ? 'text-foreground/90' : 'text-foreground/80'
      )}>
        {lines.map((line, i) => (
          <span key={i}>
            {line}
            {i < lines.length - 1 && <br />}
          </span>
        ))}
        {showCaret && (
          <span
            className={cn(
              'inline-block w-1.5 h-3 bg-streaming ml-0.5 align-middle rounded-[1px]',
              'shadow-[0_0_8px_hsl(var(--streaming))] transition-opacity duration-[400ms]',
              message.isStreaming ? 'animate-blink opacity-100' : 'opacity-0',
            )}
          />
        )}
      </div>

      <div className="flex items-center gap-3 mt-0.5 opacity-0 group-hover/msg:opacity-100 transition-opacity">
        <span className="font-mono text-[9px] text-muted-foreground/30">{message.id.slice(0, 8)}</span>
        {(message.isStreaming || message.streamComplete === true) && (
          <span className={cn(
            'font-mono text-[9px]',
            message.isStreaming ? 'text-streaming/70' : 'text-muted-foreground/30'
          )}>
            {message.isStreaming ? 'STREAMING' : 'COMPLETE'}
          </span>
        )}
        <button
          onClick={handleCopy}
          className="ml-auto flex items-center gap-1 font-mono text-[9px] text-muted-foreground/30 hover:text-muted-foreground transition-colors"
        >
          {copied ? <Check size={9} /> : <Copy size={9} />}
          {copied ? 'copied' : 'copy'}
        </button>
      </div>
    </div>
  );
}

function formatTime(date: Date): string {
  return date.toLocaleTimeString('en-US', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  });
}
