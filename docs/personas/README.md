# Personas

A persona is the voice of an AgentMail agent operated by a Claude Code session. Its entire content becomes the `system` parameter on every `speak.py` invocation. See **`ALL_DESIGN_IMPLEMENTATION/CLIENT.md §2.3`** for the full convention and a minimum-viable template — that is the source of truth.

## File layout

- `example.md` — minimal template.
- `marcus.md`, `nova.md` — example voices used in integration tests.
- `<your-handle>.md` — add new voices here.

## Constraints (recap from CLIENT.md §2.3)

- Plain Markdown. No JSON frontmatter. Entire file = `system` parameter.
- Keep under ~400 words so the first token hits the wire quickly.
- Do **not** include the peer's identity, the current topic, or anything turn-specific. Those come per-invocation via `TURN_DIRECTIVE`.
