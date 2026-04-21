import { useEffect, useRef, useState } from 'react';
import { Conversation, Agent } from '@/lib/types';
import { Hash, Users } from 'lucide-react';
import { cn } from '@/lib/utils';

interface ConversationHeaderProps {
  conversation: Conversation;
  agents: Agent[];
}

/** Returns `true` for `durationMs` after `value` increases. Local helper. */
function useIncrementFlash(value: number, durationMs = 250): boolean {
  const prev = useRef(value);
  const [flashing, setFlashing] = useState(false);
  useEffect(() => {
    if (value > prev.current) {
      setFlashing(true);
      const t = setTimeout(() => setFlashing(false), durationMs);
      prev.current = value;
      return () => clearTimeout(t);
    }
    prev.current = value;
    return undefined;
  }, [value, durationMs]);
  return flashing;
}

/** Remounts on value change so the `ticker-roll` keyframe re-triggers. */
function Ticker({ value, className }: { value: number | string; className?: string }) {
  return (
    <span key={String(value)} className={cn('inline-block animate-ticker-roll tabular-nums', className)}>
      {value}
    </span>
  );
}

export function ConversationHeader({ conversation, agents }: ConversationHeaderProps) {
  const members = agents.filter(a => conversation.memberIds.includes(a.id));
  const streaming = members.filter(m => m.status === 'streaming');
  const online = members.filter(m => m.status === 'online');
  const onlineCount = online.length + streaming.length;
  const onlineFlash = useIncrementFlash(onlineCount);
  const msgsFlash = useIncrementFlash(conversation.messageCount);

  return (
    <div className="h-12 border-b border-border px-4 flex items-center justify-between shrink-0 bg-surface-2">
      <div className="flex items-center gap-2.5">
        <Hash size={13} className="text-muted-foreground/60 shrink-0" />
        <span className="font-mono text-sm font-semibold text-foreground/90">
          {conversation.name.replace('# ', '')}
        </span>
        {conversation.description && (
          <>
            <div className="w-px h-4 bg-border mx-1" />
            <span className="font-mono text-xs text-muted-foreground/50 truncate max-w-xs">
              {conversation.description}
            </span>
          </>
        )}
      </div>

      <div className="flex items-center gap-4">
        {streaming.length > 0 && (
          <div className="flex items-center gap-1.5">
            <div className="flex gap-0.5">
              {[0, 1, 2].map(i => (
                <div
                  key={i}
                  className="w-1 h-1 rounded-full bg-streaming streaming-dot"
                  style={{ animationDelay: `${i * 0.16}s` }}
                />
              ))}
            </div>
            <span className="font-mono text-[10px] text-streaming">
              <Ticker value={streaming.length} /> streaming
            </span>
          </div>
        )}

        <div className="flex items-center gap-1.5">
          <Users size={11} className="text-muted-foreground/50" />
          <span
            className={cn(
              'font-mono text-[10px] px-1 rounded transition-colors duration-200',
              onlineFlash
                ? 'bg-primary/80 text-primary-foreground'
                : 'text-muted-foreground/60',
            )}
          >
            <Ticker value={onlineCount} />/<Ticker value={members.length} /> online
          </span>
        </div>

        <div className="flex items-center gap-1.5">
          <span
            className={cn(
              'font-mono text-[10px] px-1 rounded transition-colors duration-200 tabular-nums',
              msgsFlash
                ? 'bg-primary/80 text-primary-foreground'
                : 'text-muted-foreground/40',
            )}
          >
            <Ticker value={conversation.messageCount} /> msgs
          </span>
        </div>
      </div>
    </div>
  );
}
