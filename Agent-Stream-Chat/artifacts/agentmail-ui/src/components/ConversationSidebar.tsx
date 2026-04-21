import { Hash, MessageSquare, Radio } from 'lucide-react';
import type { Conversation } from '@/lib/types';
import { cn } from '@/lib/utils';

interface ConversationSidebarProps {
  conversations: Conversation[];
  activeId: string;
  onSelect: (id: string) => void;
  streamingIds: Set<string>;
  unreadIds: Set<string>;
}

export function ConversationSidebar({
  conversations,
  activeId,
  onSelect,
  streamingIds,
  unreadIds,
}: ConversationSidebarProps) {
  // Sort: streaming first, then unread, then by recency.
  const sorted = [...conversations].sort((a, b) => {
    const aStream = streamingIds.has(a.id) ? 1 : 0;
    const bStream = streamingIds.has(b.id) ? 1 : 0;
    if (aStream !== bStream) return bStream - aStream;
    const aUnread = unreadIds.has(a.id) ? 1 : 0;
    const bUnread = unreadIds.has(b.id) ? 1 : 0;
    if (aUnread !== bUnread) return bUnread - aUnread;
    return b.lastActivity.getTime() - a.lastActivity.getTime();
  });

  return (
    <div className="w-56 bg-surface-1 border-r border-border flex flex-col shrink-0">
      {/* Brand header */}
      <div className="h-12 flex items-center gap-2.5 px-4 border-b border-border bg-surface-0">
        <div className="w-5 h-5 rounded bg-primary/20 border border-primary/40 flex items-center justify-center shrink-0">
          <MessageSquare size={10} className="text-primary" />
        </div>
        <span className="font-mono text-xs font-semibold text-foreground tracking-widest uppercase">AgentChat</span>
        <span className="font-mono text-[10px] text-muted-foreground/40 ml-0.5">v1.0</span>
      </div>

      {/* Scrollable channels */}
      <div className="flex-1 overflow-y-auto py-2">
        <SectionHeader label="Conversations" count={sorted.length} />
        <div className="space-y-px mt-1">
          {sorted.map(conv => (
            <ConvItem
              key={conv.id}
              conv={conv}
              isActive={conv.id === activeId}
              isStreaming={streamingIds.has(conv.id)}
              isUnread={unreadIds.has(conv.id)}
              onClick={() => onSelect(conv.id)}
            />
          ))}
        </div>
      </div>
    </div>
  );
}

function SectionHeader({ label, count, className }: { label: string; count?: number; className?: string }) {
  return (
    <div className={cn('flex items-center justify-between px-3 py-1', className)}>
      <span className="font-mono text-[10px] text-muted-foreground/60 uppercase tracking-widest">{label}</span>
      {typeof count === 'number' && (
        <span className="font-mono text-[10px] text-muted-foreground/40 tabular-nums">{count}</span>
      )}
    </div>
  );
}

function ConvItem({
  conv,
  isActive,
  isStreaming,
  isUnread,
  onClick,
}: {
  conv: Conversation;
  isActive: boolean;
  isStreaming: boolean;
  isUnread: boolean;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      className={cn(
        'w-full flex items-center gap-2 px-3 py-1.5 mx-1 rounded transition-colors text-left relative',
        'font-mono text-xs',
        isActive
          ? 'bg-accent text-foreground'
          : isStreaming
          ? 'text-streaming hover:bg-streaming/10'
          : isUnread
          ? 'text-foreground hover:bg-accent/40 font-semibold'
          : 'text-muted-foreground hover:bg-accent/40 hover:text-foreground/80',
      )}
    >
      {/* Left edge marker for unread (solid) / streaming (animated) */}
      {(isStreaming || isUnread) && !isActive && (
        <span
          className={cn(
            'absolute left-0 top-1.5 bottom-1.5 w-0.5 rounded-r',
            isStreaming ? 'bg-streaming animate-pulse' : 'bg-primary',
          )}
        />
      )}
      {isStreaming ? (
        <Radio size={11} className="shrink-0 text-streaming animate-pulse" />
      ) : (
        <Hash size={11} className={cn('shrink-0', isUnread ? 'opacity-90' : 'opacity-60')} />
      )}
      <span className="truncate flex-1 text-[11px]">
        {conv.name.replace('# ', '')}
      </span>
      {isStreaming ? (
        <span className="font-mono text-[9px] text-streaming tabular-nums shrink-0 uppercase tracking-wider">
          live
        </span>
      ) : isUnread ? (
        <span className="ml-1 inline-flex items-center justify-center min-w-[1rem] h-4 px-1 rounded-full bg-primary text-primary-foreground font-mono text-[9px] tabular-nums shrink-0">
          new
        </span>
      ) : (
        <span className="font-mono text-[9px] text-muted-foreground/40 tabular-nums shrink-0">
          {conv.headSeq}
        </span>
      )}
    </button>
  );
}
