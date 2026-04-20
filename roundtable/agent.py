"""ConcurrentAgent — one persona running autonomously against AgentMail.

Each agent owns three units of work, all running concurrently:

  1. `SSESubscriber` thread. Always-on tail of the shared conversation.
     Reassembles messages and hands completed ones to the agent's inbox.
     Advances `ack_seq` after each finalized message.

  2. Decision loop (this thread). Blocks on the inbox, applies a cheap
     keyword + turn-count heuristic to decide whether to speak in response
     to each new message, adds jitter, and — if it decides to speak —
     hands off to the writer.

  3. Writer (inline in the decision loop). Constructs the transcript,
     calls Anthropic's streaming messages API, pipes the token deltas into
     `AgentMailClient.stream_send_with_factory`. Crucially this path does
     NOT hold the decision loop's position in the inbox — if a second
     message arrives while writing, it is queued and processed after.

Agents never coordinate with each other directly. Two agents can hit
`/messages/stream` at the same instant; the server and the SSE fanout are
what serializes the perceived timeline. This matches §2 of CLIENT.md:
"Concurrent writers interleave."
"""

from __future__ import annotations

import json
import queue
import re
import sys
import threading
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Callable, Iterator

from roundtable.client import AgentMailClient, AssembledMessage, SSESubscriber

ANTHROPIC_URL = "https://api.anthropic.com/v1/messages"
DEFAULT_MODEL = "claude-sonnet-4-6"
DEFAULT_MAX_TOKENS = 512

CONSENSUS_RE = re.compile(r"\bCONSENSUS:\s*(.+)$", re.IGNORECASE | re.MULTILINE)


# ── persona config ────────────────────────────────────────────────────────


@dataclass(frozen=True)
class PersonaConfig:
    """Static config that drives an agent's behavior.

    - `role`: user-facing tag printed into the transcript (e.g. "ARCHITECT").
    - `system_prompt`: loaded from the persona markdown.
    - `keywords`: lowercase substrings; any match in a peer's message is a
      strong signal to speak.
    - `min_gap`: minimum number of peer messages this agent will let pass
      without speaking before the idle rule fires.
    - `jitter_range`: seconds to wait after deciding to speak, to let the
      panel settle and reduce (but not eliminate) concurrent writes.
    - `allow_consensus`: only Product is allowed to emit the CONSENSUS signal.
    """

    role: str
    system_prompt: str
    keywords: tuple[str, ...]
    min_gap: int
    jitter_range: tuple[float, float]
    allow_consensus: bool = False


# ── anthropic streaming ───────────────────────────────────────────────────


def stream_claude(api_key: str, system: str, transcript: str, model: str = DEFAULT_MODEL) -> Iterator[str]:
    """Call the Anthropic messages API with stream=true, yield text deltas.

    We use urllib rather than the SDK to keep dependencies minimal and to
    make the streaming path trivially inspectable.
    """
    user_content = (
        f"Full conversation so far (oldest first):\n\n{transcript}\n\n"
        "Produce your next turn now, following your output format exactly."
        if transcript
        else "The panel is convening. Produce your opening turn now."
    )
    payload = json.dumps(
        {
            "model": model,
            "max_tokens": DEFAULT_MAX_TOKENS,
            "stream": True,
            "system": system,
            "messages": [{"role": "user", "content": user_content}],
        }
    ).encode()
    req = urllib.request.Request(
        ANTHROPIC_URL,
        data=payload,
        headers={
            "content-type": "application/json",
            "x-api-key": api_key,
            "anthropic-version": "2023-06-01",
        },
    )
    try:
        resp = urllib.request.urlopen(req, timeout=120)
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"Anthropic API error {e.code}: {body}") from e

    for raw in resp:
        line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
        if not line.startswith("data:"):
            continue
        data = line[5:].strip()
        if not data or data == "[DONE]":
            continue
        try:
            ev = json.loads(data)
        except json.JSONDecodeError:
            continue
        if ev.get("type") == "content_block_delta":
            delta = ev.get("delta") or {}
            if delta.get("type") == "text_delta":
                text = delta.get("text", "")
                if text:
                    yield text


# ── ConcurrentAgent ───────────────────────────────────────────────────────


@dataclass
class ConcurrentAgent:
    """One persona running autonomously on AgentMail.

    Lifecycle:
      * `attach(client, agent_id, conv_id, stop_event, shared)` wires the
        runtime context. This is separate from construction so the driver
        can register all agents before any of them starts reading.
      * `start()` spins up the SSE thread and the decision loop thread.
      * `join()` waits for both to exit.
      * `stop_event.set()` triggers a graceful shutdown — both threads exit
        on their next wake.
    """

    persona: PersonaConfig
    api_key: str

    # ── runtime (set via attach) ──────────────────────────────────────────
    client: AgentMailClient | None = None
    agent_id: str = ""
    conv_id: str = ""
    stop_event: threading.Event | None = None
    shared: "SharedPanelState | None" = None

    # ── mutable per-agent state ───────────────────────────────────────────
    _inbox: "queue.Queue[AssembledMessage]" = field(default_factory=queue.Queue)
    _transcript: list[AssembledMessage] = field(default_factory=list)
    _transcript_lock: threading.Lock = field(default_factory=threading.Lock)
    _last_spoke_idx: int = -1  # index into self._transcript at which I last spoke
    _sse_thread: SSESubscriber | None = None
    _loop_thread: threading.Thread | None = None

    # ── wiring ────────────────────────────────────────────────────────────

    def attach(
        self,
        client: AgentMailClient,
        agent_id: str,
        conv_id: str,
        stop_event: threading.Event,
        shared: "SharedPanelState",
    ) -> None:
        self.client = client
        self.agent_id = agent_id
        self.conv_id = conv_id
        self.stop_event = stop_event
        self.shared = shared

    def prime_from_history(self) -> None:
        """Load any pre-existing complete messages so we don't re-process them.

        If the driver posted a seed message before our SSE started, we want
        the reassembled history AND we want to skip reacting to our own
        prior turns. Uses ?from=0 to get everything in ascending order.
        """
        assert self.client is not None
        snap = self.client.history(self.conv_id, self.agent_id, from_seq=0, limit=100)
        for m in snap.get("messages", []):
            if m.get("status") != "complete":
                continue
            asm = AssembledMessage(
                message_id=m["message_id"],
                sender_id=m["sender_id"],
                content=m["content"],
                seq_start=m["seq_start"],
                seq_end=m["seq_end"],
                status="complete",
            )
            with self._transcript_lock:
                self._transcript.append(asm)
                if asm.sender_id == self.agent_id:
                    self._last_spoke_idx = len(self._transcript) - 1

    # ── public control ────────────────────────────────────────────────────

    def start(self) -> None:
        assert self.client and self.stop_event is not None and self.shared is not None

        self._sse_thread = SSESubscriber(
            client=self.client,
            agent_id=self.agent_id,
            conv_id=self.conv_id,
            stop_event=self.stop_event,
            on_message=self._on_message,
            on_member=self._on_member,
            on_error=self._on_sse_error,
        )
        self._sse_thread.start()

        self._loop_thread = threading.Thread(target=self._decision_loop, name=f"loop-{self.persona.role}", daemon=True)
        self._loop_thread.start()

    def join(self) -> None:
        if self._sse_thread is not None:
            self._sse_thread.join()
        if self._loop_thread is not None:
            self._loop_thread.join()

    # ── SSE callbacks ─────────────────────────────────────────────────────

    def _on_message(self, msg: AssembledMessage) -> None:
        # Append to transcript under lock (decision loop also reads).
        with self._transcript_lock:
            self._transcript.append(msg)
            idx = len(self._transcript) - 1
            if msg.sender_id == self.agent_id:
                self._last_spoke_idx = idx
                # Let the shared panel know we've finished a turn — Product
                # relies on per-role speak counts to gate CONSENSUS.
                assert self.shared is not None
                self.shared.record_turn(self.persona.role)
                # Parse for CONSENSUS signal (only matters if this agent emitted it).
                if self.persona.allow_consensus:
                    m = CONSENSUS_RE.search(msg.content)
                    if m:
                        self.shared.declare_consensus(self.persona.role, m.group(1).strip())
                        assert self.stop_event is not None
                        self.stop_event.set()
                # We don't need to react to our own message.
                return

        # Ack every finalized message — including our own — via seq_end.
        # Per §4.11 this is synchronous + regression-guarded, cheap.
        try:
            assert self.client is not None
            self.client.ack(self.conv_id, self.agent_id, msg.seq_end)
        except Exception as e:  # noqa: BLE001
            print(f"[{self.persona.role}] ack failed @ seq {msg.seq_end}: {e}", file=sys.stderr)

        # Queue for the decision loop.
        self._inbox.put(msg)

    def _on_member(self, joined: bool, who: str) -> None:
        verb = "joined" if joined else "left"
        print(f"[{self.persona.role}] membership: {who[:8]} {verb}", flush=True)

    def _on_sse_error(self, fatal: bool, detail: str) -> None:
        tag = "fatal" if fatal else "transient"
        print(f"[{self.persona.role}] sse {tag}: {detail}", file=sys.stderr, flush=True)
        if fatal:
            assert self.stop_event is not None
            self.stop_event.set()

    # ── decision loop ─────────────────────────────────────────────────────

    def _decision_loop(self) -> None:
        assert self.stop_event is not None and self.shared is not None
        while not self.stop_event.is_set():
            try:
                msg = self._inbox.get(timeout=0.5)
            except queue.Empty:
                # While idle, still check the idle rule — if the room has
                # moved on and this agent hasn't contributed in a while, jump
                # in without a specific trigger.
                if self._should_speak_idle():
                    self._speak(trigger=None)
                continue

            if self.stop_event.is_set():
                return

            # Skip speaking if this is membership or our own message.
            if msg.sender_id == self.agent_id:
                continue

            if self._should_speak(msg):
                # Jitter before streaming — enough to let other agents either
                # take the turn (we'll see their message_start on SSE) or not.
                jitter_lo, jitter_hi = self.persona.jitter_range
                delay = jitter_lo + (jitter_hi - jitter_lo) * _rand()
                deadline = time.monotonic() + delay
                while time.monotonic() < deadline:
                    if self.stop_event.is_set():
                        return
                    time.sleep(min(0.1, deadline - time.monotonic()))
                # Re-check: has someone else taken the turn since we queued?
                if not self._still_relevant(msg):
                    continue
                self._speak(trigger=msg)

    # ── speak gates ───────────────────────────────────────────────────────

    def _should_speak(self, msg: AssembledMessage) -> bool:
        # Keyword trigger: any of this persona's lane keywords appear in the
        # fresh message from a peer.
        body = msg.content.lower()
        if any(kw in body for kw in self.persona.keywords):
            return True
        # Idle rule: fall through to the same check the empty-inbox path uses.
        return self._should_speak_idle()

    def _should_speak_idle(self) -> bool:
        with self._transcript_lock:
            if not self._transcript:
                return False
            last_idx = len(self._transcript) - 1
            if self._last_spoke_idx == last_idx:
                return False
            gap = last_idx - max(self._last_spoke_idx, 0)
        return gap >= self.persona.min_gap

    def _still_relevant(self, msg: AssembledMessage) -> bool:
        """After jitter, confirm the reason we chose to speak still holds.

        If another agent spoke AFTER `msg` and addressed the same keyword
        more recently, defer — they just took our turn.
        """
        with self._transcript_lock:
            for later in self._transcript[msg.seq_end :]:  # noqa: E203 -- seq_end used as coarse cutoff; loop below is source of truth
                pass
            # Walk the transcript tail looking for messages newer than msg.
            newer: list[AssembledMessage] = []
            for m in reversed(self._transcript):
                if m.seq_end <= msg.seq_end:
                    break
                newer.append(m)
        if not newer:
            return True
        # If any of those newer messages were from peers AND carried our
        # keywords, the opportunity has moved on.
        for later in newer:
            if later.sender_id == self.agent_id:
                continue
            if any(kw in later.content.lower() for kw in self.persona.keywords):
                return False
        return True

    # ── writer ────────────────────────────────────────────────────────────

    def _speak(self, trigger: AssembledMessage | None) -> None:
        assert self.client is not None and self.shared is not None
        # Snapshot the transcript. Doing this under lock is safe because the
        # SSE thread appends while holding the same lock.
        with self._transcript_lock:
            transcript_snapshot = list(self._transcript)
        rendered = self._render_transcript(transcript_snapshot)

        def body() -> Iterator[str]:
            # Mirror tokens to stdout so the operator sees the stream live.
            prefix = f"\n── {self.persona.role} ──────────────────────────\n"
            sys.stdout.write(prefix)
            sys.stdout.flush()
            try:
                for tok in stream_claude(self.api_key, self.persona.system_prompt, rendered):
                    sys.stdout.write(tok)
                    sys.stdout.flush()
                    yield tok
            except Exception as e:  # noqa: BLE001
                # If Anthropic fails mid-stream, let the body generator return
                # cleanly; AgentMail will see a normal EOF and finalize the
                # message with whatever content arrived. (If no content
                # arrived, AgentMail finalizes an empty complete message — an
                # edge case we tolerate since it's rare and harmless.)
                sys.stderr.write(f"\n[{self.persona.role}] anthropic error: {e}\n")
                sys.stderr.flush()

        try:
            self.client.stream_send_with_factory(self.conv_id, self.agent_id, body)
        except Exception as e:  # noqa: BLE001
            print(f"\n[{self.persona.role}] stream_send failed: {e}", file=sys.stderr, flush=True)

    def _render_transcript(self, msgs: list[AssembledMessage]) -> str:
        """Render the transcript for the LLM prompt with role tags.

        We look up the speaker's persona role via the shared panel state —
        agents need to know who said what (the architect should respond to
        "the security reviewer" rather than "agent 7a8b"). Unknown senders
        (e.g. legacy inbound) are rendered as OUTSIDE.
        """
        assert self.shared is not None
        lines: list[str] = []
        for m in msgs:
            if m.sender_id == self.agent_id:
                tag = "YOU"
            else:
                tag = self.shared.role_for(m.sender_id) or "OUTSIDE"
            status_note = "" if m.status == "complete" else f"  [aborted: {m.abort_reason or 'unknown'}]"
            lines.append(f"{tag}{status_note}:\n{m.content.strip()}")
        return "\n\n".join(lines)


# ── shared panel state ────────────────────────────────────────────────────


class SharedPanelState:
    """Cross-agent coordination that is NOT state-of-the-conversation.

    The panel's notion of who-is-who, how many times each persona has spoken
    (for the CONSENSUS gate), and the consensus outcome. Agents do not
    directly synchronize through this — they only read/write their own slots.

    Critically: this is process-local and NEVER used to short-circuit the
    AgentMail-mediated communication. Every agent still receives messages
    via SSE. This is purely about prompt rendering and termination.
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._agent_to_role: dict[str, str] = {}
        self._turns: dict[str, int] = {}
        self._consensus: tuple[str, str] | None = None

    def register(self, agent_id: str, role: str) -> None:
        with self._lock:
            self._agent_to_role[agent_id] = role
            self._turns[role] = 0

    def role_for(self, agent_id: str) -> str | None:
        with self._lock:
            return self._agent_to_role.get(agent_id)

    def record_turn(self, role: str) -> None:
        with self._lock:
            self._turns[role] = self._turns.get(role, 0) + 1

    def turns(self) -> dict[str, int]:
        with self._lock:
            return dict(self._turns)

    def declare_consensus(self, role: str, summary: str) -> None:
        with self._lock:
            if self._consensus is None:
                self._consensus = (role, summary)

    def consensus(self) -> tuple[str, str] | None:
        with self._lock:
            return self._consensus


# ── utils ─────────────────────────────────────────────────────────────────


def _rand() -> float:
    import random

    return random.random()
