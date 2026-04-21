/**
 * React Query hook that loads a page of history for a single conversation
 * and adapts the rows into UI `Message` objects sorted by S2 sequence
 * ascending. The resulting cache is merged with live SSE state by
 * `useConversationStream` (via `mergeHistoryAndLive`) at render time.
 */

import { useQuery } from '@tanstack/react-query';
import { getObserverHistory } from '@/lib/api';
import { toUIMessage, type Message } from '@/lib/types';

/** Build a stable query key for a conversation's history. */
export const MESSAGES_QUERY_KEY = (cid: string) =>
  ['messages', cid] as const;

/** A sentinel key used when the hook is disabled (no active conversation). */
const MESSAGES_DISABLED_KEY = ['messages', '__disabled__'] as const;

/** Subscribe to the most recent page of history for the given conversation. */
export function useMessages(cid: string | null) {
  return useQuery<Message[]>({
    enabled: !!cid,
    queryKey: cid ? MESSAGES_QUERY_KEY(cid) : MESSAGES_DISABLED_KEY,
    queryFn: async () => {
      if (!cid) return [];
      const res = await getObserverHistory(cid, { limit: 100 });
      const ui = res.messages.map((m) => toUIMessage(m, cid));
      ui.sort((a, b) => a.seq - b.seq);
      return ui;
    },
    staleTime: 5_000,
  });
}
