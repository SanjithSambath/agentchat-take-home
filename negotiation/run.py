#!/usr/bin/env python3
"""
Drive a 2019 Honda Civic negotiation between two Claude personas whose only
communication channel is the AgentMail HTTP + SSE protocol.

Each persona runs as a distinct AgentMail agent (its own X-Agent-ID). Every
turn is POSTed to `POST /v1/conversations/:cid/messages/stream` as NDJSON so
each Anthropic token is appended to S2 as a `message_append` event the
instant it arrives — the UI's SSE subscriber sees the reply materialize live,
character by character. Each persona reads the other party's prior turns
exclusively through `GET /v1/conversations/:cid/messages` (history) so the
drivers have no backchannel.

Usage:
    python3 negotiation/run.py
    python3 negotiation/run.py --base-url http://localhost:8080 --max-turns 30

Requirements:
    - AgentMail server running (default: http://localhost:8080)
    - ANTHROPIC_API_KEY env var or ANTHROPIC_API_KEY= line in repo .env
    - `requests` Python package (already present in stdlib-adjacent envs)
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Iterator

import requests

# ── Config ──────────────────────────────────────────────────────────────────

MAX_TURNS = 30
ANTHROPIC_MODEL = "claude-sonnet-4-6"
MAX_TOKENS = 512
ANTHROPIC_URL = "https://api.anthropic.com/v1/messages"
DEFAULT_BASE_URL = "http://localhost:8080"

REPO_ROOT = Path(__file__).resolve().parent.parent
AGENTS_DIR = REPO_ROOT / ".claude" / "agents"

SIGNAL_RE = {
    "ACCEPT": re.compile(r"\bACCEPT:\s*\$?([\d,]+)", re.IGNORECASE),
    "OFFER": re.compile(r"\bOFFER:\s*\$?([\d,]+)", re.IGNORECASE),
    "WALKAWAY": re.compile(r"\bWALKAWAY\b", re.IGNORECASE),
}

BANNER = "─" * 60


# ── UUIDv7 (AgentMail rejects any other version on message_id) ─────────────


def uuidv7() -> str:
    """Generate a UUIDv7 string per RFC 9562 §5.7.

    AgentMail's send endpoints validate the 4-bit version field and reject
    any id that isn't v7 (stable ordering + dedup retry safety).
    """
    ms = int(time.time() * 1000)
    rand = os.urandom(10)
    b = bytearray(16)
    b[0] = (ms >> 40) & 0xFF
    b[1] = (ms >> 32) & 0xFF
    b[2] = (ms >> 24) & 0xFF
    b[3] = (ms >> 16) & 0xFF
    b[4] = (ms >> 8) & 0xFF
    b[5] = ms & 0xFF
    b[6] = 0x70 | (rand[0] & 0x0F)  # version = 7
    b[7] = rand[1]
    b[8] = 0x80 | (rand[2] & 0x3F)  # variant = 10
    b[9] = rand[3]
    b[10:16] = rand[4:10]
    hx = b.hex()
    return f"{hx[0:8]}-{hx[8:12]}-{hx[12:16]}-{hx[16:20]}-{hx[20:32]}"


# ── Persona + secrets loading ──────────────────────────────────────────────


def load_persona(name: str) -> str:
    path = AGENTS_DIR / f"{name}.md"
    text = path.read_text()
    if text.startswith("---"):
        _, _, body = text.split("---", 2)
        return body.strip()
    return text.strip()


def read_api_key() -> str:
    key = os.environ.get("ANTHROPIC_API_KEY")
    if key:
        return key.strip()
    env_file = REPO_ROOT / ".env"
    if env_file.exists():
        for line in env_file.read_text().splitlines():
            if line.strip().startswith("ANTHROPIC_API_KEY"):
                _, _, value = line.partition("=")
                return value.strip().strip('"').strip("'")
    raise SystemExit("ANTHROPIC_API_KEY not set and not in .env")


# ── AgentMail HTTP client ───────────────────────────────────────────────────


class AgentMail:
    """Thin wrapper over the AgentMail HTTP API — one instance per process."""

    def __init__(self, base_url: str):
        self.base = base_url.rstrip("/")
        self.session = requests.Session()

    def health(self) -> None:
        r = self.session.get(f"{self.base}/health", timeout=5)
        r.raise_for_status()

    def create_agent(self) -> str:
        r = self.session.post(f"{self.base}/agents", timeout=10)
        r.raise_for_status()
        return r.json()["agent_id"]

    def create_conversation(self, agent_id: str) -> str:
        r = self.session.post(
            f"{self.base}/conversations",
            headers={"X-Agent-ID": agent_id},
            timeout=10,
        )
        r.raise_for_status()
        return r.json()["conversation_id"]

    def invite(self, conv_id: str, inviter_id: str, invitee_id: str) -> None:
        r = self.session.post(
            f"{self.base}/conversations/{conv_id}/invite",
            headers={
                "X-Agent-ID": inviter_id,
                "Content-Type": "application/json",
            },
            data=json.dumps({"agent_id": invitee_id}),
            timeout=10,
        )
        r.raise_for_status()

    def history(self, conv_id: str, agent_id: str) -> list[dict[str, Any]]:
        """Return the full transcript in ascending seq order."""
        r = self.session.get(
            f"{self.base}/conversations/{conv_id}/messages",
            headers={"X-Agent-ID": agent_id},
            params={"from": 0, "limit": 100},
            timeout=10,
        )
        r.raise_for_status()
        msgs = r.json().get("messages", [])
        msgs.sort(key=lambda m: m["seq_start"])
        return [m for m in msgs if m.get("status") == "complete"]

    def stream_send(
        self,
        conv_id: str,
        agent_id: str,
        token_iter: Iterator[str],
    ) -> str:
        """POST an NDJSON-streamed message. Returns the generated message_id.

        The body is emitted lazily: the header frame goes out first, then one
        `{"content": "..."}` line per token yielded by `token_iter`. Requests
        sends this with Transfer-Encoding: chunked so AgentMail appends each
        chunk to S2 as a `message_append` event in real time.
        """
        message_id = uuidv7()

        def body() -> Iterator[bytes]:
            yield (json.dumps({"message_id": message_id}) + "\n").encode()
            for token in token_iter:
                if not token:
                    continue
                yield (json.dumps({"content": token}) + "\n").encode()

        r = self.session.post(
            f"{self.base}/conversations/{conv_id}/messages/stream",
            headers={
                "X-Agent-ID": agent_id,
                "Content-Type": "application/x-ndjson",
            },
            data=body(),
            timeout=300,
        )
        r.raise_for_status()
        return message_id


# ── Anthropic streaming ─────────────────────────────────────────────────────


def stream_claude(api_key: str, system: str, transcript: str) -> Iterator[str]:
    """Call the Anthropic messages API with stream=true, yield text deltas."""
    user_content = (
        f"Negotiation transcript so far:\n\n{transcript}\n\n"
        "Produce your next turn now, following your output format exactly."
        if transcript
        else "A potential counterparty has reached out. Begin the negotiation with your opening turn."
    )
    payload = json.dumps(
        {
            "model": ANTHROPIC_MODEL,
            "max_tokens": MAX_TOKENS,
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
        raise SystemExit(f"Anthropic API error {e.code}: {body}")

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


# ── Transcript rendering + signal parsing ──────────────────────────────────


def build_transcript(
    messages: list[dict[str, Any]], speaker_id: str
) -> str:
    """Render AgentMail history as the user-prompt transcript for a persona."""
    lines: list[str] = []
    for m in messages:
        label = "YOU" if m["sender_id"] == speaker_id else "OTHER PARTY"
        lines.append(f"{label}:\n{m['content']}")
    return "\n\n".join(lines)


def parse_signal(reply: str) -> tuple[str, int | None]:
    m = SIGNAL_RE["ACCEPT"].search(reply)
    if m:
        return "ACCEPT", int(m.group(1).replace(",", ""))
    m = SIGNAL_RE["OFFER"].search(reply)
    if m:
        return "OFFER", int(m.group(1).replace(",", ""))
    if SIGNAL_RE["WALKAWAY"].search(reply):
        return "WALKAWAY", None
    return "NONE", None


# ── Main driver ─────────────────────────────────────────────────────────────


def run_turn(
    mail: AgentMail,
    api_key: str,
    conv_id: str,
    speaker_id: str,
    system: str,
) -> tuple[str, str, int | None]:
    """Fetch history from AgentMail, call Claude, stream tokens back to AgentMail."""
    history = mail.history(conv_id, speaker_id)
    transcript = build_transcript(history, speaker_id)

    collected: list[str] = []

    def tee() -> Iterator[str]:
        for token in stream_claude(api_key, system, transcript):
            collected.append(token)
            # Mirror tokens to stdout so the operator sees the turn stream live
            # alongside the UI. Flushing is important — token deltas are tiny.
            sys.stdout.write(token)
            sys.stdout.flush()
            yield token

    mail.stream_send(conv_id, speaker_id, tee())
    sys.stdout.write("\n")
    sys.stdout.flush()

    reply = "".join(collected).strip()
    sig, amount = parse_signal(reply)
    return reply, sig, amount


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--base-url",
        default=os.environ.get("AGENTMAIL_BASE_URL", DEFAULT_BASE_URL),
        help="AgentMail server base URL (default: %(default)s)",
    )
    ap.add_argument(
        "--max-turns",
        type=int,
        default=MAX_TURNS,
        help="Safety cap on turn count (default: %(default)s)",
    )
    ap.add_argument(
        "--observer",
        action="append",
        default=[],
        metavar="AGENT_ID",
        help=(
            "Extra agent UUID to invite as a silent observer so it appears in "
            "that agent's conversation list. Paste your UI agent id here (grab "
            "it from devtools: localStorage.getItem('agentmail.agent_id')). "
            "May be given multiple times."
        ),
    )
    args = ap.parse_args()

    api_key = read_api_key()
    systems = {
        "seller": load_persona("seller-negotiator"),
        "buyer": load_persona("buyer-negotiator"),
    }
    mail = AgentMail(args.base_url)

    try:
        mail.health()
    except Exception as e:
        raise SystemExit(
            f"AgentMail unreachable at {args.base_url} ({e}). "
            f"Start it with `make run` in another terminal."
        )

    seller_id = mail.create_agent()
    buyer_id = mail.create_agent()
    conv_id = mail.create_conversation(seller_id)
    mail.invite(conv_id, seller_id, buyer_id)
    for obs in args.observer:
        mail.invite(conv_id, seller_id, obs)

    print(f"{BANNER}")
    print(f"Negotiation: 2019 Honda Civic LX")
    print(f"  conversation : {conv_id}")
    print(f"  seller       : {seller_id}")
    print(f"  buyer        : {buyer_id}")
    if args.observer:
        print(f"  observers    : {', '.join(args.observer)}")
    print(f"  transport    : AgentMail HTTP + SSE only")
    print(f"{BANNER}")

    order = [("seller", seller_id), ("buyer", buyer_id)]
    last_offer: dict[str, int | None] = {"buyer": None, "seller": None}

    for turn in range(1, args.max_turns + 1):
        name, aid = order[(turn - 1) % 2]
        other_name = order[turn % 2][0]

        print(f"\n── Turn {turn:02d} · {name.upper():<6} ─────────────────────────")
        # Retry transient network stalls (e.g. Anthropic read timeouts) in place.
        # The partial message on the server side stays in_progress and is filtered
        # out of history, so a fresh retry produces a clean complete message.
        last_err: Exception | None = None
        for attempt in range(1, 4):
            try:
                reply, sig, amount = run_turn(
                    mail, api_key, conv_id, aid, systems[name]
                )
                last_err = None
                break
            except (requests.exceptions.RequestException, urllib.error.URLError, TimeoutError) as e:
                last_err = e
                print(f"\n[warn] turn {turn} attempt {attempt} failed: {e}; retrying…")
                time.sleep(2 * attempt)
        if last_err is not None:
            raise SystemExit(f"turn {turn} failed after 3 attempts: {last_err}")

        if sig == "WALKAWAY":
            print(f"\n{BANNER}\n[NO DEAL] {name} walked away after {turn} turns.\n{BANNER}")
            return 2

        if sig == "OFFER":
            last_offer[name] = amount

        if sig == "ACCEPT":
            if amount is not None and last_offer[other_name] == amount:
                print(f"\n{BANNER}\n[DEAL] Agreed at ${amount:,} after {turn} turns.\n{BANNER}")
                return 0
            # Accept at a number the other side did not offer — treat as
            # the accepter's new standing offer and keep going.
            last_offer[name] = amount

        if sig == "NONE":
            print("[warn] turn had no OFFER/ACCEPT/WALKAWAY signal; continuing anyway")

    print(f"\n{BANNER}\n[TIMEOUT] Hit {args.max_turns}-turn cap without a deal.\n{BANNER}")
    return 3


if __name__ == "__main__":
    sys.exit(main())
