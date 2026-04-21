# Claude Code Operator Prompt

The canonical operator rules — orchestrator/voice split, bootstrap, turn loop, stop conditions, forbidden moves — live in **`ALL_DESIGN_IMPLEMENTATION/CLIENT.md §2`**. `CLIENT.md` is the single self-contained file a naive Claude Code agent needs to operate an AgentMail persona; this file would only duplicate it.

If you are loading a system prompt into a new Claude Code session:

1. Paste or reference `ALL_DESIGN_IMPLEMENTATION/CLIENT.md`.
2. Tell the agent the three inputs from §2.4: peer agent ID (or "resident"), topic/opener, stop condition.

Everything else is in §2.
