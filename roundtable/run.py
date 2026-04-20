#!/usr/bin/env python3
"""Roundtable: four AgentMail-powered Claude personas arguing over a design RFC.

Unlike `negotiation/run.py` — which is strict turn-taking between two agents —
this driver spins up four autonomous agents that read and write the shared
conversation concurrently. Each agent owns an SSE subscription and decides
for itself when to speak. Concurrent writes interleave at the service layer,
exactly as described in CLIENT.md §2 ("Concurrent writers interleave. Two
messages in flight produce events like S1 A1 S2 A1 A2 E1 E2 on the wire.").

Session shape:

  1. Register four distinct agents (Architect, Performance, Security, Product).
  2. Product creates the conversation and invites the other three.
  3. Product posts the RFC framing as the opening message.
  4. All four agents attach their SSE subscribers and start their decision
     loops simultaneously. From this point there is no turn order — who
     speaks when is determined by each agent's persona-specific heuristics
     and jitter.
  5. The session ends when Product emits `CONSENSUS: ...` OR a safety cap
     (max messages / wall-clock) trips.

Usage:
    python3 -m roundtable.run
    python3 -m roundtable.run --base-url http://localhost:8080
    python3 -m roundtable.run --rfc "Migrate checkout to event-sourced ledger"
    python3 -m roundtable.run --observer <agent_id>     # invite a silent UI watcher
"""

from __future__ import annotations

import argparse
import json
import os
import signal
import sys
import threading
import time
from pathlib import Path

from roundtable.agent import ConcurrentAgent, PersonaConfig, SharedPanelState
from roundtable.client import AgentMailClient, uuidv7

# ── config ────────────────────────────────────────────────────────────────

DEFAULT_BASE_URL = "http://localhost:8080"
DEFAULT_MAX_MESSAGES = 20
DEFAULT_MAX_WALL_SECONDS = 600

REPO_ROOT = Path(__file__).resolve().parent.parent
PERSONAS_DIR = REPO_ROOT / ".claude" / "agents"

BANNER = "─" * 64

DEFAULT_RFC = (
    "We are being asked to add a new capability to AgentMail: cross-region "
    "read replicas of conversation streams, so agents in a remote region can "
    "tail a conversation with single-digit-millisecond added latency. Current "
    "architecture is a single-region S2 basin plus Postgres for metadata. "
    "The ask is: (a) design the replica fanout; (b) decide what consistency "
    "we offer to readers; (c) identify the top failure mode we must defend "
    "against; and (d) name what we deliberately cut to ship in one quarter. "
    "Please weigh in from your respective lanes. Architect first, then "
    "Performance and Security in parallel, then I'll steer toward convergence."
)


# ── persona loading ───────────────────────────────────────────────────────


def load_persona_body(name: str) -> str:
    path = PERSONAS_DIR / f"{name}.md"
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


def build_personas(api_key: str) -> list[ConcurrentAgent]:
    """Construct the four ConcurrentAgent instances.

    Keyword lanes and min_gap values are tuned so that, on any given peer
    message, typically 0–1 agents see a strong trigger — but the idle rule
    eventually forces everyone to chime in. jitter_range prevents deterministic
    ordering without adding a hard coordination point.
    """
    return [
        ConcurrentAgent(
            persona=PersonaConfig(
                role="ARCHITECT",
                system_prompt=load_persona_body("roundtable-architect"),
                keywords=(
                    "boundary",
                    "contract",
                    "api",
                    "invariant",
                    "state machine",
                    "schema",
                    "coupling",
                    "abstraction",
                    "interface",
                    "module",
                ),
                min_gap=2,
                jitter_range=(0.4, 1.6),
            ),
            api_key=api_key,
        ),
        ConcurrentAgent(
            persona=PersonaConfig(
                role="PERFORMANCE",
                system_prompt=load_persona_body("roundtable-performance"),
                keywords=(
                    "latency",
                    "throughput",
                    "cache",
                    "index",
                    "fanout",
                    "p95",
                    "p99",
                    "slow",
                    "fast",
                    "db",
                    "query",
                    "hot path",
                    "backpressure",
                ),
                min_gap=2,
                jitter_range=(0.6, 2.0),
            ),
            api_key=api_key,
        ),
        ConcurrentAgent(
            persona=PersonaConfig(
                role="SECURITY",
                system_prompt=load_persona_body("roundtable-security"),
                keywords=(
                    "auth",
                    "token",
                    "secret",
                    "pii",
                    "attacker",
                    "abuse",
                    "injection",
                    "replay",
                    "audit",
                    "trust",
                    "leak",
                    "tenant",
                ),
                min_gap=2,
                jitter_range=(0.5, 1.8),
            ),
            api_key=api_key,
        ),
        ConcurrentAgent(
            persona=PersonaConfig(
                role="PRODUCT",
                system_prompt=load_persona_body("roundtable-product"),
                keywords=(
                    "user",
                    "timeline",
                    "ship",
                    "scope",
                    "customer",
                    "revenue",
                    "deadline",
                    "quarter",
                    "launch",
                    "cut",
                ),
                # Product lets the engineers argue first — longer idle tolerance.
                min_gap=3,
                jitter_range=(1.0, 2.5),
                allow_consensus=True,
            ),
            api_key=api_key,
        ),
    ]


# ── driver ────────────────────────────────────────────────────────────────


def seed_rfc(client: AgentMailClient, agent_id: str, conv_id: str, rfc: str) -> None:
    """Product posts the opening message via the NON-streaming endpoint.

    Using the complete-send path here is deliberate — the RFC is a fixed
    string, not a token stream. It exercises §4.7 (the plain JSON endpoint)
    in the same run that exercises §4.8 (NDJSON streaming) for the reply
    turns. Good coverage, same production client.
    """
    mid = uuidv7()
    resp = client.session.post(
        f"{client.base_url}/conversations/{conv_id}/messages",
        headers=client._headers(agent_id, **{"Content-Type": "application/json"}),
        data=json.dumps({"message_id": mid, "content": rfc}).encode(),
        timeout=30,
    )
    resp.raise_for_status()


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--base-url", default=os.environ.get("AGENTMAIL_BASE_URL", DEFAULT_BASE_URL))
    ap.add_argument("--rfc", default=DEFAULT_RFC, help="RFC framing posted by the Product agent")
    ap.add_argument(
        "--max-messages",
        type=int,
        default=DEFAULT_MAX_MESSAGES,
        help="Safety cap on total messages before forcing shutdown",
    )
    ap.add_argument(
        "--max-seconds",
        type=int,
        default=DEFAULT_MAX_WALL_SECONDS,
        help="Wall-clock cap before forcing shutdown",
    )
    ap.add_argument(
        "--observer",
        action="append",
        default=[],
        metavar="AGENT_ID",
        help="Extra agent UUID to invite as a silent observer (for the UI). May repeat.",
    )
    args = ap.parse_args()

    api_key = read_api_key()
    client = AgentMailClient(base_url=args.base_url)

    # Probe health but treat 503 as a warning rather than fatal — the server
    # reports 503 when any downstream check is degraded, and S2 can flap
    # briefly without affecting our writes. A connection error IS fatal.
    try:
        h = client.health()
        print(f"[health] {h}")
    except Exception as e:  # noqa: BLE001
        msg = str(e)
        if msg.startswith("503"):
            print(f"[health] degraded: {msg} — proceeding anyway", file=sys.stderr)
        else:
            raise SystemExit(f"AgentMail unreachable at {args.base_url} ({e}). Start it with `make run`.")

    # Build agents BEFORE registering — fail fast if a persona file is missing.
    agents = build_personas(api_key)
    shared = SharedPanelState()

    # Register all four agents.
    for a in agents:
        aid = client.register_agent()
        a.agent_id = aid
        shared.register(aid, a.persona.role)

    # Product creates the conversation so it's the natural opener.
    product = next(a for a in agents if a.persona.role == "PRODUCT")
    conv_id = client.create_conversation(product.agent_id)

    # Invite every other agent. Loop over the other three — invite is
    # idempotent on the invitee, so observer re-invites are safe too.
    for a in agents:
        if a is product:
            continue
        client.invite(conv_id, product.agent_id, a.agent_id)
    for obs in args.observer:
        client.invite(conv_id, product.agent_id, obs)

    stop_event = threading.Event()

    # ── print session header ──────────────────────────────────────────────
    print(BANNER)
    print("Roundtable: AgentMail 4-agent concurrent design review")
    print(f"  base_url     : {args.base_url}")
    print(f"  conversation : {conv_id}")
    for a in agents:
        print(f"  {a.persona.role:<11}: {a.agent_id}")
    if args.observer:
        print(f"  observers    : {', '.join(args.observer)}")
    print(f"  max_messages : {args.max_messages}")
    print(f"  max_seconds  : {args.max_seconds}")
    print(f"  transport    : AgentMail HTTP (CRUD) + NDJSON stream writes + SSE reads")
    print(BANNER)

    # ── seed the RFC (Product, non-streaming send) ────────────────────────
    print(f"\n── RFC ─ {product.persona.role} (seed via POST /messages) ────────")
    print(args.rfc)
    seed_rfc(client, product.agent_id, conv_id, args.rfc)

    # ── attach + prime + start ────────────────────────────────────────────
    for a in agents:
        a.attach(client, a.agent_id, conv_id, stop_event, shared)
        a.prime_from_history()
    for a in agents:
        a.start()

    # ── ctrl-c propagates stop ────────────────────────────────────────────
    def _on_signal(signum, frame):  # noqa: ARG001
        print("\n[signal] stopping…", flush=True)
        stop_event.set()

    signal.signal(signal.SIGINT, _on_signal)
    signal.signal(signal.SIGTERM, _on_signal)

    # ── supervise ────────────────────────────────────────────────────────
    start_wall = time.monotonic()
    exit_code = 0
    try:
        while not stop_event.is_set():
            turns = shared.turns()
            total = sum(turns.values())
            if total >= args.max_messages:
                print(f"\n[cap] reached {total} messages — stopping", flush=True)
                exit_code = 3
                stop_event.set()
                break
            if time.monotonic() - start_wall >= args.max_seconds:
                print(f"\n[cap] reached wall-clock cap — stopping", flush=True)
                exit_code = 3
                stop_event.set()
                break
            time.sleep(1.0)
    finally:
        stop_event.set()
        for a in agents:
            a.join()

    # ── summary ──────────────────────────────────────────────────────────
    print(f"\n{BANNER}")
    consensus = shared.consensus()
    if consensus is not None:
        role, summary = consensus
        print(f"[CONSENSUS via {role}] {summary}")
        exit_code = 0
    elif exit_code != 0:
        pass
    else:
        print("[END] stop signal without explicit consensus")
    final_turns = shared.turns()
    for role in ("ARCHITECT", "PERFORMANCE", "SECURITY", "PRODUCT"):
        print(f"  {role:<11}: {final_turns.get(role, 0)} turn(s)")
    print(BANNER)
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
