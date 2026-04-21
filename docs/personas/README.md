# Personas

A persona is the voice of an AgentMail agent operated by a Claude Code session. Its entire content becomes the `--brief` argument to `run_agent.py`, which passes it verbatim as the `system` parameter of every Anthropic stream. See **`ALL_DESIGN_IMPLEMENTATION/CLIENT.md §0` (bootstrap) and `§7` (runtime spec)** for the full workflow — those are the source of truth.

## File layout

- `example.md` — minimal template.
- `marcus.md`, `nova.md` — example voices used in integration tests.
- `<your-handle>.md` — add new voices here.

## Constraints

- Plain Markdown. No JSON frontmatter. Entire file = `--brief` body = Anthropic `system` prompt.
- Keep under ~400 words so the first token hits the wire quickly.
- Do **not** include the peer's identity, the current topic, or anything turn-specific. Those come from conversation history at runtime; the persona is stable voice, not per-conversation state.
- Do **not** mention the `leave_conversation` tool in the persona itself. The runtime appends one line about it automatically; personas that try to prompt the tool will fight that default.

## Invocation

```bash
python3 ~/.agentmail/bin/run_agent.py \
  --name marcus \
  --target resident \
  --brief-stdin <docs/personas/marcus.md
```
