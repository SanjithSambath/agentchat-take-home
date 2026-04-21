import { useEffect, useRef, useState } from 'react';
import type { Agent, AgentStatus, Conversation } from '@/lib/types';
import { cn } from '@/lib/utils';
import { Cpu, Clock, WifiOff, Radio } from 'lucide-react';

interface MemberListProps {
  conversation: Conversation;
  agents: Agent[];
}

const STATUS_ORDER: AgentStatus[] = ['streaming', 'online', 'idle', 'offline'];

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

export function MemberList({ conversation, agents }: MemberListProps) {
  const members = agents
    .filter(a => conversation.memberIds.includes(a.id))
    .sort((a, b) => {
      const ai = STATUS_ORDER.indexOf(a.status);
      const bi = STATUS_ORDER.indexOf(b.status);
      return ai - bi;
    });

  const streaming = members.filter(m => m.status === 'streaming');
  const online = members.filter(m => m.status === 'online');
  const idle = members.filter(m => m.status === 'idle');
  const offline = members.filter(m => m.status === 'offline');

  const membersFlash = useIncrementFlash(members.length);

  return (
    <div className="w-52 bg-surface-1 border-l border-border flex flex-col shrink-0">
      {/* Header */}
      <div className="h-12 flex items-center px-4 border-b border-border">
        <span
          className={cn(
            'font-mono text-[10px] uppercase tracking-widest px-1 rounded transition-colors duration-200',
            membersFlash ? 'bg-primary/80 text-primary-foreground' : 'text-muted-foreground/60',
          )}
        >
          Members — <Ticker value={members.length} />
        </span>
      </div>

      <div className="flex-1 overflow-y-auto py-3 space-y-4">
        {streaming.length > 0 && (
          <StatusGroup label="Streaming" icon={<Radio size={9} />} color="text-streaming" members={streaming} />
        )}
        {online.length > 0 && (
          <StatusGroup label="Online" icon={<Cpu size={9} />} color="text-online" members={online} />
        )}
        {idle.length > 0 && (
          <StatusGroup label="Idle" icon={<Clock size={9} />} color="text-idle" members={idle} />
        )}
        {offline.length > 0 && (
          <StatusGroup label="Offline" icon={<WifiOff size={9} />} color="text-offline" members={offline} />
        )}
      </div>

      {/* Conversation metadata */}
      <div className="border-t border-border p-3 space-y-2">
        <div className="font-mono text-[9px] text-muted-foreground/40 uppercase tracking-widest">Conversation</div>
        <div className="space-y-1">
          <MetaRow label="id" value={conversation.id.replace('conv-', 'conv_')} />
          <MetaRow label="msgs" value={String(conversation.messageCount)} />
          <MetaRow label="members" value={String(members.length)} />
          <MetaRow
            label="created"
            value={conversation.createdAt.toISOString().slice(0, 10)}
          />
        </div>
      </div>
    </div>
  );
}

function StatusGroup({
  label,
  icon,
  color,
  members,
}: {
  label: string;
  icon: React.ReactNode;
  color: string;
  members: Agent[];
}) {
  return (
    <div>
      <div className={cn('flex items-center gap-1.5 px-3 mb-1', color)}>
        {icon}
        <span className="font-mono text-[9px] uppercase tracking-widest opacity-80">
          {label} — <Ticker value={members.length} />
        </span>
      </div>
      <div className="space-y-px">
        {members.map(agent => (
          <AgentRow key={agent.id} agent={agent} />
        ))}
      </div>
    </div>
  );
}

function AgentRow({ agent }: { agent: Agent }) {
  const statusColor =
    agent.status === 'online' ? 'bg-online' :
    agent.status === 'streaming' ? 'bg-streaming' :
    agent.status === 'idle' ? 'bg-idle' :
    'bg-offline';

  return (
    <div className="flex items-center gap-2.5 px-3 py-1.5 hover:bg-accent/30 rounded mx-1 transition-colors cursor-default group">
      <div className="relative shrink-0">
        <div className={cn(
          'w-7 h-7 rounded-sm flex items-center justify-center border',
          agent.status === 'offline'
            ? 'bg-muted/40 border-border/50'
            : agent.status === 'streaming'
            ? 'bg-streaming/10 border-streaming/30'
            : 'bg-muted border-border'
        )}>
          <span className={cn(
            'font-mono text-[9px] font-bold',
            agent.status === 'offline' ? 'text-muted-foreground/40' :
            agent.status === 'streaming' ? 'text-streaming' :
            'text-muted-foreground'
          )}>
            {agent.avatar}
          </span>
        </div>
        <div className={cn(
          'absolute -bottom-0.5 -right-0.5 w-2 h-2 rounded-full border-2 border-surface-1',
          statusColor,
          agent.status === 'streaming' && 'animate-pulse'
        )} />
      </div>

      <div className="flex-1 min-w-0">
        <div className={cn(
          'font-mono text-[10px] truncate leading-tight',
          agent.status === 'offline' ? 'text-muted-foreground/40' : 'text-foreground/80'
        )}>
          {agent.name}
        </div>
        <div className={cn(
          'font-mono text-[9px] truncate',
          agent.status === 'offline' ? 'text-muted-foreground/25' : 'text-muted-foreground/50'
        )}>
          {agent.model}
        </div>
      </div>
    </div>
  );
}

function MetaRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center gap-2">
      <span className="font-mono text-[9px] text-muted-foreground/40 w-14 shrink-0">{label}</span>
      <span className="font-mono text-[9px] text-muted-foreground/70 truncate">{value}</span>
    </div>
  );
}
