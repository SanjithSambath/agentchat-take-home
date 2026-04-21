/**
 * Owns the per-browser agent identity used by every authenticated request.
 *
 * Persists the id in `localStorage` under a single key and provides a
 * bootstrap helper that POSTs to `/agents` exactly once per browser profile.
 */

const KEY = 'agentmail.agent_id';

/** Base URL for all backend calls — overridable via `VITE_API_BASE_URL`. */
export const API_BASE: string =
  (import.meta.env.VITE_API_BASE_URL as string | undefined) ?? '/api';

/** Read the cached agent id, or `null` if none is persisted yet. */
export function getAgentId(): string | null {
  try {
    return localStorage.getItem(KEY);
  } catch {
    return null;
  }
}

/** Persist the agent id for this browser. Silently no-ops on storage errors. */
export function setAgentId(id: string): void {
  try {
    localStorage.setItem(KEY, id);
  } catch {
    // ignore storage errors (e.g., private mode quota)
  }
}

/**
 * Return the cached agent id, or bootstrap a new one by calling `POST /agents`
 * and persisting the result. Throws on network or shape errors.
 */
export async function ensureAgentId(): Promise<string> {
  const existing = getAgentId();
  if (existing) return existing;
  const res = await fetch(`${API_BASE}/agents`, { method: 'POST' });
  if (!res.ok) throw new Error(`agent bootstrap failed: ${res.status}`);
  const body = (await res.json()) as { agent_id?: string } | null;
  if (!body?.agent_id) throw new Error('agent bootstrap: missing agent_id');
  setAgentId(body.agent_id);
  return body.agent_id;
}
