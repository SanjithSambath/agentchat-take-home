#!/usr/bin/env python3
# run_agent.py — AgentMail Claude Code daemon.
#
# One process that:
#   1. registers or resumes an agent identity under ~/.agentmail/<name>/
#   2. resolves a target (resident | peer:<uuid> | join:<cid>)
#   3. tails the conversation's SSE stream
#   4. on each peer message_end, opens a fresh Anthropic stream with a
#      `leave_conversation` tool bound and pipes text tokens straight into
#      the NDJSON body of POST /conversations/{cid}/messages/stream
#   5. exits cleanly when the LLM calls `leave_conversation`, when all
#      peers have left, when SIGINT/SIGTERM fires, on SSE fatal, or when
#      the --turns safety cap is reached
#
# Invariants:
#   - Tokens cross the wire as Anthropic emits them. No buffering.
#   - The SSE tail is armed before the first outbound send.
#   - Idempotent on message_id (UUIDv7). Retries honor the error table.
#   - Stdout is a scoreboard; the conversation transcript lives in the UI.
#
# Canonical contract: ALL_DESIGN_IMPLEMENTATION/CLIENT.md §7.
import argparse
import json
import os
import queue
import random
import re
import signal
import sys
import threading
import time
from pathlib import Path

import requests
from anthropic import Anthropic


__VERSION__ = "2026-04-21.2"


# ─────────────────────────────────────────────────────────────────────
# Small utilities
# ─────────────────────────────────────────────────────────────────────

def uuid7() -> str:
    ts = int(time.time() * 1000).to_bytes(6, "big")
    r = bytearray(os.urandom(10))
    r[0] = (r[0] & 0x0F) | 0x70  # version 7
    r[2] = (r[2] & 0x3F) | 0x80  # RFC 4122 variant
    h = (ts + bytes(r)).hex()
    return f"{h[0:8]}-{h[8:12]}-{h[12:16]}-{h[16:20]}-{h[20:32]}"


_UUID_RE = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$",
    re.IGNORECASE,
)


def is_uuid(s: str) -> bool:
    return bool(_UUID_RE.match(s or ""))


def need_env(name: str) -> str:
    v = os.environ.get(name, "").strip()
    if not v:
        sys.exit(f"run_agent.py: missing env {name}")
    return v


def log(line: str) -> None:
    # Scoreboard output. Stderr so stdout stays machine-parseable for the
    # final summary JSON line.
    sys.stderr.write(line + "\n")
    sys.stderr.flush()


# ─────────────────────────────────────────────────────────────────────
# State directory
# ─────────────────────────────────────────────────────────────────────

def state_dir(name: str) -> Path:
    base = Path.home() / ".agentmail" / name
    try:
        base.mkdir(parents=True, exist_ok=True)
        return base
    except Exception:
        fb = Path("/tmp") / f"agentmail_{name}"
        log(f"run_agent.py: ~/.agentmail/{name}/ not writable, falling back to {fb}")
        fb.mkdir(parents=True, exist_ok=True)
        return fb


def file_read(p: Path) -> str | None:
    try:
        return p.read_text().strip() if p.exists() else None
    except Exception:
        return None


def file_write(p: Path, v: str) -> None:
    try:
        tmp = p.with_suffix(p.suffix + ".tmp")
        tmp.write_text(v)
        tmp.replace(p)
    except Exception as e:
        log(f"run_agent.py: could not persist {p}: {e}")


def archive_conv_id(p: Path) -> None:
    # Move an existing conv.id aside so a fresh conversation can be created.
    # Keeps every past conversation reachable from state by inspection.
    if not p.exists():
        return
    stamp = int(time.time())
    bak = p.with_suffix(p.suffix + f".bak.{stamp}")
    try:
        p.replace(bak)
    except Exception as e:
        log(f"run_agent.py: could not archive {p} → {bak}: {e}")


# ─────────────────────────────────────────────────────────────────────
# HTTP helpers
# ─────────────────────────────────────────────────────────────────────

def http_json(method: str, url: str, agent: str | None = None, **kw):
    headers = kw.pop("headers", {})
    if agent:
        headers["X-Agent-ID"] = agent
    r = requests.request(method, url, headers=headers, timeout=30, **kw)
    r.raise_for_status()
    return r.json() if r.content else {}


def fetch_members(base: str, agent: str, cid: str) -> list[str]:
    try:
        r = http_json("GET", f"{base}/conversations/{cid}", agent)
        return list(r.get("members") or [])
    except Exception:
        return []


def fetch_history(base: str, agent: str, cid: str, limit: int = 100) -> list[dict]:
    r = http_json("GET", f"{base}/conversations/{cid}/messages", agent,
                  params={"limit": limit})
    return list(reversed(r.get("messages", []) or []))


def post_leave(base: str, agent: str, cid: str) -> None:
    try:
        requests.post(
            f"{base}/conversations/{cid}/leave",
            headers={"X-Agent-ID": agent},
            timeout=10,
        )
    except Exception:
        pass


# ─────────────────────────────────────────────────────────────────────
# Identity + target + conversation resolution
# ─────────────────────────────────────────────────────────────────────

def register_or_resume(base: str, id_path: Path) -> str:
    aid = file_read(id_path)
    if aid:
        return aid
    aid = http_json("POST", f"{base}/agents")["agent_id"]
    file_write(id_path, aid)
    return aid


class Target:
    __slots__ = ("kind", "value")

    def __init__(self, kind: str, value: str | None):
        self.kind = kind      # "resident" | "peer" | "join"
        self.value = value    # peer UUID (peer), conversation UUID (join), None (resident)


def parse_target(raw: str) -> Target:
    if raw == "resident":
        return Target("resident", None)
    if raw.startswith("peer:"):
        v = raw[len("peer:"):].strip().lower()
        if not is_uuid(v):
            sys.exit(f"run_agent.py: --target peer:<uuid> requires a UUID; got {v!r}")
        return Target("peer", v)
    if raw.startswith("join:"):
        v = raw[len("join:"):].strip().lower()
        if not is_uuid(v):
            sys.exit(f"run_agent.py: --target join:<cid> requires a UUID; got {v!r}")
        return Target("join", v)
    sys.exit("run_agent.py: --target must be 'resident', 'peer:<uuid>', or 'join:<cid>'")


def resolve_peer_and_conversation(
    base: str, agent: str, target: Target, state: Path,
) -> tuple[str | None, str, bool]:
    """Return (peer_id, conversation_id, is_initiator).

    is_initiator=True means "if history is empty, we speak first."

    For target=resident or peer:<id>: archive any prior conv.id, create fresh
    conversation, invite peer, return (peer_id, cid, True).

    For target=join:<cid>: verify membership, return (None, cid, False).
    """
    conv_id_path = state / "conv.id"

    if target.kind == "join":
        cid = target.value
        members = fetch_members(base, agent, cid)
        if agent not in members:
            sys.exit(
                f"run_agent.py: not a member of conversation {cid}. "
                f"Ask the initiator to invite your agent first: "
                f"POST /conversations/{cid}/invite with body {{\"agent_id\":\"{agent}\"}}."
            )
        file_write(conv_id_path, cid)
        return (None, cid, False)

    # resident | peer — always fresh conversation
    peer_id: str | None = None
    if target.kind == "resident":
        try:
            peer_id = http_json("GET", f"{base}/agents/resident")["agent_id"]
        except Exception as e:
            sys.exit(f"run_agent.py: resident unavailable: {e}")
    else:
        peer_id = target.value

    archive_conv_id(conv_id_path)
    cid = http_json("POST", f"{base}/conversations", agent)["conversation_id"]
    file_write(conv_id_path, cid)

    try:
        http_json(
            "POST", f"{base}/conversations/{cid}/invite", agent,
            json={"agent_id": peer_id},
        )
    except requests.HTTPError as e:
        sys.exit(f"run_agent.py: failed to invite peer {peer_id}: {e}")

    return (peer_id, cid, True)


# ─────────────────────────────────────────────────────────────────────
# SSE tail thread
# ─────────────────────────────────────────────────────────────────────

class Tail(threading.Thread):
    """Tails GET /conversations/{cid}/stream, reassembles messages by
    message_id, and emits one completed-event at a time on out_q.

    Emits kinds: "message" | "aborted" | "joined" | "left" | "fatal".
    Events authored by self are filtered out inside _emit.
    """

    def __init__(self, base: str, agent: str, cid: str, state_file: Path,
                 out_q: queue.Queue, stop_evt: threading.Event):
        super().__init__(daemon=True)
        self.base, self.agent, self.cid = base, agent, cid
        self.state, self.out, self.stop = state_file, out_q, stop_evt
        self.armed = threading.Event()
        self.last = self._load_last()

    def _load_last(self) -> int | None:
        try:
            return int(self.state.read_text().strip()) if self.state.exists() else None
        except Exception:
            return None

    def _save_last(self, seq: int) -> None:
        try:
            tmp = self.state.with_suffix(self.state.suffix + ".tmp")
            tmp.write_text(str(seq))
            tmp.replace(self.state)
        except Exception:
            pass

    def run(self) -> None:
        # Finite 45s read timeout as a watchdog: if the SSE socket goes
        # silent longer than one heartbeat window (server beats every 30s),
        # ReadTimeout fires, we reconnect from Last-Event-ID. Handles
        # silently-buffered streams through Cloudflare Tunnel + requests.
        seen: dict[int, bool] = {}
        pending: dict[str, dict] = {}
        attempt = 0

        while not self.stop.is_set():
            try:
                h = {"X-Agent-ID": self.agent, "Accept": "text/event-stream"}
                if self.last is not None:
                    h["Last-Event-ID"] = str(self.last)
                with requests.get(
                    f"{self.base}/conversations/{self.cid}/stream",
                    headers=h, stream=True, timeout=(10, 45),
                ) as r:
                    if r.status_code in (400, 403, 404):
                        self.out.put({"kind": "fatal",
                                      "msg": f"{r.status_code}: {r.text[:200]}"})
                        return
                    r.raise_for_status()
                    attempt = 0
                    self.armed.set()
                    seq = etype = data = None
                    for raw in r.iter_lines(decode_unicode=True):
                        if self.stop.is_set():
                            return
                        if raw is None:
                            continue
                        if raw == "":
                            if (etype and data is not None and seq is not None
                                    and seq not in seen):
                                seen[seq] = True
                                if len(seen) > 4096:
                                    seen.pop(next(iter(seen)))
                                try:
                                    self._emit(etype, json.loads(data), seq, pending)
                                except ValueError:
                                    pass
                                self.last = seq
                                self._save_last(seq)
                            seq = etype = data = None
                            continue
                        if raw.startswith(":"):
                            # :ok handshake or :heartbeat — both arm the tail
                            self.armed.set()
                            continue
                        key, _, val = raw.partition(": ")
                        if key == "id":
                            try:
                                seq = int(val)
                            except ValueError:
                                seq = None
                        elif key == "event":
                            etype = val
                        elif key == "data":
                            data = val
            except requests.RequestException:
                pass
            attempt += 1
            time.sleep(min(30.0, 0.5 * 2 ** attempt) * random.random())

    def _emit(self, etype: str, d: dict, seq: int, pending: dict) -> None:
        if etype == "message_start":
            pending[d["message_id"]] = {"sender": d.get("sender_id", ""), "c": []}
            return
        if etype == "message_append":
            e = pending.get(d["message_id"])
            if e:
                e["c"].append(d.get("content", ""))
            return
        if etype in ("message_end", "message_abort"):
            e = pending.pop(d["message_id"], None)
            if not e or e["sender"] == self.agent:
                return
            kind = "message" if etype == "message_end" else "aborted"
            self.out.put({
                "kind": kind, "seq": seq,
                "sender": e["sender"], "content": "".join(e["c"]),
            })
            return
        if etype in ("agent_joined", "agent_left"):
            aid = d.get("agent_id", "")
            if not aid or aid == self.agent:
                return
            self.out.put({
                "kind": "joined" if etype == "agent_joined" else "left",
                "agent_id": aid,
            })


# ─────────────────────────────────────────────────────────────────────
# History → Anthropic messages
# ─────────────────────────────────────────────────────────────────────

def history_to_messages(hist: list[dict], me: str) -> list[dict]:
    """Reconstruct an Anthropic-shaped messages list from AgentMail history.

    Consecutive same-role messages are merged with newline separation
    because Anthropic's API requires alternating user/assistant roles.
    Leading non-user messages are dropped so the first message is always
    user-role.
    """
    out: list[dict] = []
    for m in hist:
        content = (m.get("content") or "").strip()
        if not content:
            continue
        role = "assistant" if m.get("sender_id") == me else "user"
        if out and out[-1]["role"] == role:
            out[-1]["content"] += "\n\n" + content
        else:
            out.append({"role": role, "content": content})
    while out and out[0]["role"] != "user":
        out.pop(0)
    return out


# ─────────────────────────────────────────────────────────────────────
# Voice: speak with leave_conversation tool bound
# ─────────────────────────────────────────────────────────────────────

TOOL_LEAVE = {
    "name": "leave_conversation",
    "description": (
        "Call this tool when you believe the task is complete, the "
        "conversation has reached a natural end, or you have nothing "
        "further to contribute. This leaves the conversation cleanly. "
        "Prefer calling it after a brief closing message."
    ),
    "input_schema": {
        "type": "object",
        "properties": {
            "reason": {
                "type": "string",
                "description": "One short sentence on why you are leaving.",
            },
        },
        "required": [],
    },
}

LEAVE_TOOL_PROMPT = (
    "\n\nYou have a tool named `leave_conversation`. Call it when you "
    "believe the task described above is complete, when the conversation "
    "has reached a natural end, or when you have nothing further to "
    "contribute. Ideally send a brief closing message first, then call the "
    "tool in the same turn."
)

PEERS_LEFT_SUFFIX = (
    "\n\nNote: all other members have left this conversation. There is "
    "nobody to reply to your next message. Call `leave_conversation` now."
)


def speak(
    base: str, agent: str, cid: str,
    system_prompt: str, model: str, max_tokens: int,
    directive: str | None = None,
    peers_left: bool = False,
) -> dict:
    """Run one voice turn. Returns dict:
        {"text": str, "leave_requested": bool, "leave_reason": str | None,
         "ok": bool, "error_code": str | None}
    """
    msgs = history_to_messages(fetch_history(base, agent, cid), agent)
    if directive:
        tag = "[Operator directive] " + directive
        if msgs and msgs[-1]["role"] == "user":
            msgs[-1]["content"] += "\n\n" + tag
        else:
            msgs.append({"role": "user", "content": tag})
    if not msgs:
        msgs = [{"role": "user", "content": "begin."}]

    system = system_prompt + LEAVE_TOOL_PROMPT
    if peers_left:
        system += PEERS_LEFT_SUFFIX

    mid = uuid7()

    # We retry the outbound POST on transient wire errors. Each retry is a
    # fresh Anthropic stream because LLM output is non-deterministic; same
    # message_id only when the prior attempt was in-flight (idempotency).
    for attempt in range(6):
        sent_text: list[str] = []
        state = {"leave_requested": False, "leave_reason": None}

        def body():
            yield (json.dumps({"message_id": mid}) + "\n").encode()
            with Anthropic().messages.stream(
                model=model, max_tokens=max_tokens,
                system=system, messages=msgs, tools=[TOOL_LEAVE],
            ) as s:
                tool_input_buf: list[str] = []
                for ev in s:
                    etype = getattr(ev, "type", "")
                    if etype == "content_block_start":
                        cb = getattr(ev, "content_block", None)
                        if cb and getattr(cb, "type", "") == "tool_use":
                            if getattr(cb, "name", "") == "leave_conversation":
                                state["leave_requested"] = True
                    elif etype == "content_block_delta":
                        delta = getattr(ev, "delta", None)
                        if delta is None:
                            continue
                        dtype = getattr(delta, "type", "")
                        if dtype == "text_delta":
                            text = getattr(delta, "text", "") or ""
                            if text:
                                sent_text.append(text)
                                yield (json.dumps({"content": text}) + "\n").encode()
                        elif dtype == "input_json_delta":
                            piece = getattr(delta, "partial_json", "") or ""
                            if piece:
                                tool_input_buf.append(piece)
                # After the stream completes, parse any accumulated tool
                # input JSON to extract `reason` (best-effort; failure is fine).
                if state["leave_requested"] and tool_input_buf:
                    try:
                        payload = json.loads("".join(tool_input_buf))
                        if isinstance(payload, dict):
                            state["leave_reason"] = payload.get("reason")
                    except Exception:
                        pass

        try:
            r = requests.post(
                f"{base}/conversations/{cid}/messages/stream",
                headers={"X-Agent-ID": agent,
                         "Content-Type": "application/x-ndjson"},
                data=body(), timeout=(10, 300),
            )
        except requests.RequestException:
            time.sleep(min(30.0, 0.5 * 2 ** (attempt + 1)) * random.random())
            continue

        if r.ok:
            return {
                "text": "".join(sent_text),
                "leave_requested": state["leave_requested"],
                "leave_reason": state["leave_reason"],
                "ok": True,
                "error_code": None,
            }

        code = ""
        try:
            code = (r.json() or {}).get("error", {}).get("code", "")
        except Exception:
            pass

        if code == "in_progress_conflict":
            time.sleep(0.3 + random.random() * 0.7)
            continue
        if code == "already_aborted":
            mid = uuid7()
            continue
        if code in ("slow_writer", "idle_timeout", "max_duration"):
            mid = uuid7()
            time.sleep(min(10.0, 0.5 * 2 ** (attempt + 1)) * random.random())
            continue
        if code == "empty_content":
            # LLM called the tool with zero preceding text. No voice to
            # publish. Signal the caller we're leaving without a message.
            return {
                "text": "",
                "leave_requested": state["leave_requested"] or True,
                "leave_reason": state["leave_reason"],
                "ok": False,
                "error_code": code,
            }
        if r.status_code >= 500 or code in ("internal_error", "timeout"):
            time.sleep(min(30.0, 0.5 * 2 ** (attempt + 1)) * random.random())
            continue

        return {
            "text": "".join(sent_text),
            "leave_requested": state["leave_requested"],
            "leave_reason": state["leave_reason"],
            "ok": False,
            "error_code": code or f"http_{r.status_code}",
        }

    return {
        "text": "", "leave_requested": False, "leave_reason": None,
        "ok": False, "error_code": "retries_exhausted",
    }


# ─────────────────────────────────────────────────────────────────────
# Response policy
# ─────────────────────────────────────────────────────────────────────

def resolve_policy(policy: str, member_count: int) -> str:
    if policy != "auto":
        return policy
    return "always" if member_count <= 2 else "round-robin"


def should_respond(policy: str, ev: dict, me: str, recent: list[str]) -> bool:
    if ev.get("kind") not in ("message", "aborted"):
        return False
    if policy == "always":
        return True
    if policy == "round-robin":
        tail = recent[-2:]
        return not (tail and all(s == me for s in tail))
    return True


# ─────────────────────────────────────────────────────────────────────
# Brief input
# ─────────────────────────────────────────────────────────────────────

def read_brief(args) -> str:
    if args.brief:
        return args.brief.strip()
    if args.brief_stdin:
        return sys.stdin.read().strip()
    return ""


# ─────────────────────────────────────────────────────────────────────
# Scoreboard helpers
# ─────────────────────────────────────────────────────────────────────

def approx_tokens(text: str) -> int:
    # Rough (~chars/4) token estimate. Not used for billing; just for the
    # scoreboard so the operator gets a size signal without another API
    # round-trip.
    if not text:
        return 0
    return max(1, len(text) // 4)


# ─────────────────────────────────────────────────────────────────────
# Main
# ─────────────────────────────────────────────────────────────────────

def parse_args(argv: list[str]) -> argparse.Namespace:
    ap = argparse.ArgumentParser(
        description="AgentMail Claude Code daemon — listen, speak, leave.",
    )
    ap.add_argument("--selfcheck", action="store_true",
                    help="Print runtime version and exit.")
    ap.add_argument("--name",
                    help="State namespace. Required except with --selfcheck. "
                         "Persisted under ~/.agentmail/<name>/. Distinct "
                         "agents on the same machine MUST use distinct --name.")
    ap.add_argument("--target",
                    help="One of: 'resident', 'peer:<agent-uuid>', "
                         "'join:<conversation-uuid>'. Required except with "
                         "--selfcheck or --register-only.")
    ap.add_argument("--brief",
                    help="The operator's ask verbatim; becomes the voice "
                         "stream's system prompt.")
    ap.add_argument("--brief-stdin", action="store_true",
                    help="Read --brief from stdin (use for multi-line).")
    ap.add_argument("--turns", type=int, default=20,
                    help="Safety cap on turns. Primary stop is the "
                         "leave_conversation tool; this is a backstop "
                         "against runaway loops. Default 20.")
    ap.add_argument("--policy", choices=["auto", "always", "round-robin"],
                    default="auto",
                    help="Response policy. 'auto' picks 'always' for "
                         "2-member conversations and 'round-robin' for 3+.")
    ap.add_argument("--register-only", action="store_true",
                    help="Register (or resume) this agent and print its ID; "
                         "do not start a conversation. Used for cross-machine "
                         "pairing setup.")
    ap.add_argument("--model", default=os.environ.get("MODEL", "claude-sonnet-4-5"),
                    help="Anthropic model id. Defaults to claude-sonnet-4-5.")
    ap.add_argument("--max-tokens", type=int, default=1024)
    return ap.parse_args(argv)


def main() -> int:
    args = parse_args(sys.argv[1:])

    if args.selfcheck:
        print(f"run_agent.py version {__VERSION__}")
        return 0

    if not args.name:
        sys.exit("run_agent.py: --name is required")

    base = need_env("BASE_URL").rstrip("/")

    sd = state_dir(args.name)
    agent = register_or_resume(base, sd / "agent.id")

    if args.register_only:
        # Caller just wants an ID to paste elsewhere.
        print(json.dumps({
            "status": "registered",
            "agent_id": agent,
            "version": __VERSION__,
        }))
        return 0

    # From here on, we run a conversation — require the rest.
    need_env("ANTHROPIC_API_KEY")
    if not args.target:
        sys.exit("run_agent.py: --target is required (resident | peer:<uuid> | join:<cid>)")
    brief = read_brief(args)
    if not brief:
        sys.exit("run_agent.py: --brief or --brief-stdin is required. Pass "
                 "the operator's ask verbatim.")

    target = parse_target(args.target)
    peer_id, cid, is_initiator = resolve_peer_and_conversation(base, agent, target, sd)

    log(f"🟢 agent `{args.name}`  id: {agent}")
    log(f"   conversation        {cid}")
    if peer_id:
        log(f"   peer                {peer_id}")
    if target.kind == "peer":
        log(f"⚠ share this conversation id with the peer so they can join:")
        log(f"   {cid}")

    # Arm the tail before any send. The server flushes `:ok\n\n` on connect
    # so armed.wait() returns quickly over a healthy network.
    events: queue.Queue = queue.Queue()
    stop_evt = threading.Event()
    tail = Tail(
        base, agent, cid,
        sd / f"listen_{agent}_{cid}.state",
        events, stop_evt,
    )
    tail.start()
    if not tail.armed.wait(timeout=15):
        sys.exit("run_agent.py: SSE tail failed to arm within 15s")

    # Track live membership. The server returns the authoritative set at
    # this moment; agent_joined/agent_left deltas keep us current.
    members = set(fetch_members(base, agent, cid))
    members.add(agent)
    if peer_id:
        members.add(peer_id)
    peers = members - {agent}
    policy = resolve_policy(args.policy, len(members))
    log(f"   policy              {policy}   members: {len(members)}")

    # SIGINT/SIGTERM → clean leave.
    closing = False

    def shutdown(*_):
        nonlocal closing
        closing = True

    signal.signal(signal.SIGINT, shutdown)
    signal.signal(signal.SIGTERM, shutdown)

    recent: list[str] = []
    turns = 0
    exit_reason = "unknown"
    peers_gone = False

    def run_one_turn(directive: str | None) -> bool:
        """Do one voice turn. Returns True if we should exit after this turn."""
        nonlocal turns, exit_reason, peers_gone
        turns += 1
        result = speak(
            base, agent, cid,
            system_prompt=brief, model=args.model, max_tokens=args.max_tokens,
            directive=directive, peers_left=peers_gone,
        )
        toks = approx_tokens(result.get("text", ""))
        tag = "you spoke" if result.get("text") else "you spoke (silent)"
        log(f"▸ turn {turns:>2}  {tag:<20} (~{toks} tokens)")

        if result.get("leave_requested"):
            reason = result.get("leave_reason") or "task complete"
            log(f"          leave_conversation requested: {reason}")
            exit_reason = "leave_tool"
            return True

        if not result.get("ok"):
            code = result.get("error_code") or "unknown"
            log(f"✗ turn {turns:>2}  speak failed: {code}")
            exit_reason = f"speak_error:{code}"
            return True

        recent.append(agent)
        return False

    startup_history = fetch_history(base, agent, cid)
    if is_initiator and len(startup_history) == 0:
        if run_one_turn("Begin the conversation now — make the first move per your brief."):
            stop_evt.set()
            post_leave(base, agent, cid)
            return print_summary(exit_reason, turns, cid, agent)
    elif not is_initiator and startup_history:
        # Responder joining a conversation that already has content. The SSE
        # tail can only replay events still ahead of our server-side
        # delivery_seq — if the server advanced the cursor on a prior
        # (failed) connection, intermediate message_start/message_append
        # frames may be gone and we'd see an orphan message_end that
        # Tail._emit correctly drops. Seed the response from history so the
        # conversation moves forward regardless of cursor state.
        latest = startup_history[-1]
        if (latest.get("sender_id") and latest.get("sender_id") != agent
                and latest.get("status") == "complete"):
            log("  joining mid-conversation; replying to latest peer message")
            if run_one_turn(None):
                stop_evt.set()
                post_leave(base, agent, cid)
                return print_summary(exit_reason, turns, cid, agent)

    # Main event loop.
    while not closing and turns < args.turns:
        try:
            ev = events.get(timeout=1.0)
        except queue.Empty:
            continue

        kind = ev.get("kind")

        if kind == "fatal":
            log(f"✗ SSE fatal: {ev.get('msg')}")
            exit_reason = "sse_fatal"
            break

        if kind == "joined":
            aid = ev.get("agent_id", "")
            if aid:
                members.add(aid)
                log(f"+ joined  {aid}")
            continue

        if kind == "left":
            aid = ev.get("agent_id", "")
            if aid and aid in members:
                members.discard(aid)
                log(f"- left    {aid}")
            if members - {agent} == set() and not peers_gone:
                peers_gone = True
                log("  all peers have left; next turn will leave_conversation")
            continue

        if kind not in ("message", "aborted"):
            continue

        recent.append(ev["sender"])
        log(f"▸ turn {turns + 1:>2}  peer replied         "
            f"(~{approx_tokens(ev.get('content', ''))} tokens)")

        if not should_respond(policy, ev, agent, recent):
            continue

        if run_one_turn(None):
            break

        # If the only thing keeping us here is the peers-gone flag and we
        # didn't leave from the LLM, cut losses.
        if peers_gone and not closing:
            log("  peers-gone flag still set after turn; leaving")
            exit_reason = "peers_left"
            break

    else:
        # `while` exited normally → hit the safety cap.
        if turns >= args.turns and exit_reason == "unknown":
            exit_reason = "turns_cap"
            log(f"  safety cap reached ({turns}/{args.turns} turns)")

    if closing and exit_reason == "unknown":
        exit_reason = "signal"

    stop_evt.set()
    post_leave(base, agent, cid)
    return print_summary(exit_reason, turns, cid, agent)


def print_summary(reason: str, turns: int, cid: str, agent: str) -> int:
    log(f"✅ left — {turns} turn(s), reason: {reason}")
    # Single machine-parseable stdout line for integrators who want to parse
    # the final state.
    print(json.dumps({
        "status": "done",
        "reason": reason,
        "turns": turns,
        "conversation_id": cid,
        "agent_id": agent,
        "version": __VERSION__,
    }))
    return 0 if reason in ("leave_tool", "peers_left", "signal") else 0


if __name__ == "__main__":
    sys.exit(main())
