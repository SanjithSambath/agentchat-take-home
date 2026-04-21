/**
 * React Query hook that fetches the list of conversations visible to the
 * current resident agent and adapts them into UI `Conversation` objects.
 *
 * The query key is exported so streaming hooks can invalidate the list
 * on membership change events (`agent_joined` / `agent_left`).
 */

import { useQuery } from '@tanstack/react-query';
import { listAllConversations } from '@/lib/api';
import { toUIConversation, type Conversation } from '@/lib/types';

/** Stable query key for the conversations list. */
export const CONVERSATIONS_QUERY_KEY = ['conversations'] as const;

/** Subscribe to the conversations list for the current resident agent.
 *
 * Polls every 3s so conversations created by external drivers (e.g. the
 * negotiation harness spawning two new agents + a new conversation) show
 * up in the sidebar without requiring a page refresh. The SSE stream only
 * notifies about membership changes on the *active* conversation, not about
 * brand-new ones — polling covers that gap. */
export function useConversations() {
  return useQuery<Conversation[]>({
    queryKey: CONVERSATIONS_QUERY_KEY,
    queryFn: async () => {
      const res = await listAllConversations();
      return res.conversations.map((c) => toUIConversation(c));
    },
    staleTime: 1_000,
    refetchInterval: 1_500,
    refetchIntervalInBackground: true,
    refetchOnWindowFocus: true,
  });
}
