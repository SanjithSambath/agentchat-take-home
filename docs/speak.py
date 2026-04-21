#!/usr/bin/env python3
# speak.py — fresh Anthropic stream → AgentMail NDJSON write, token-by-token.
# Canonical reference lives in ALL_DESIGN_IMPLEMENTATION/CLIENT.md §2.2.
# Env: BASE_URL, AGENT_ID, CID, PERSONA_FILE, ANTHROPIC_API_KEY,
#      TURN_DIRECTIVE (opt), MODEL (opt), MAX_TOKENS (opt).
import json, os, sys, time, requests
from anthropic import Anthropic


def uuid7():
    ts = int(time.time() * 1000).to_bytes(6, "big")
    rand = bytearray(os.urandom(10))
    rand[0] = (rand[0] & 0x0F) | 0x70  # version 7
    rand[2] = (rand[2] & 0x3F) | 0x80  # RFC 4122 variant
    h = (ts + bytes(rand)).hex()
    return f"{h[0:8]}-{h[8:12]}-{h[12:16]}-{h[16:20]}-{h[20:32]}"


def need(v):
    x = os.environ.get(v, "").strip()
    if not x:
        sys.exit(f"speak.py: missing {v}")
    return x


def history(base, agent, cid, limit=100):
    r = requests.get(
        f"{base}/conversations/{cid}/messages",
        headers={"X-Agent-ID": agent},
        params={"limit": limit},
        timeout=30,
    )
    r.raise_for_status()
    return list(reversed(r.json().get("messages", []) or []))


def to_messages(hist, me, directive):
    out = []
    for m in hist:
        c = (m.get("content") or "").strip()
        if not c:
            continue
        role = "assistant" if m.get("sender_id") == me else "user"
        if out and out[-1]["role"] == role:
            out[-1]["content"] += "\n\n" + c
        else:
            out.append({"role": role, "content": c})
    if directive:
        tag = "[Operator directive] " + directive
        if out and out[-1]["role"] == "user":
            out[-1]["content"] += "\n\n" + tag
        else:
            out.append({"role": "user", "content": tag})
    while out and out[0]["role"] != "user":
        out.pop(0)
    return out


def body(mid, stream):
    yield (json.dumps({"message_id": mid}) + "\n").encode()
    for ev in stream:
        if ev.type == "content_block_delta" and getattr(ev.delta, "text", None):
            yield (json.dumps({"content": ev.delta.text}) + "\n").encode()


def main():
    base = need("BASE_URL").rstrip("/")
    agent, cid = need("AGENT_ID"), need("CID")
    persona = open(need("PERSONA_FILE"), encoding="utf-8").read().strip()
    need("ANTHROPIC_API_KEY")
    directive = os.environ.get("TURN_DIRECTIVE", "").strip()
    msgs = to_messages(history(base, agent, cid), agent, directive)
    if not msgs:
        sys.exit("speak.py: empty history and no TURN_DIRECTIVE")
    mid = uuid7()
    with Anthropic().messages.stream(
        model=os.environ.get("MODEL", "claude-sonnet-4-5"),
        max_tokens=int(os.environ.get("MAX_TOKENS", "1024")),
        system=persona,
        messages=msgs,
    ) as s:
        r = requests.post(
            f"{base}/conversations/{cid}/messages/stream",
            headers={"X-Agent-ID": agent, "Content-Type": "application/x-ndjson"},
            data=body(mid, s),
            timeout=(10, 300),
        )
    if not r.ok:
        sys.exit(f"speak.py: server {r.status_code}: {r.text[:500]}")
    print(json.dumps({"status": "ok", "message_id": mid, **r.json()}))


if __name__ == "__main__":
    main()
