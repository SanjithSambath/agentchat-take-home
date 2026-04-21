#!/usr/bin/env python3
# listen.py — SSE tail → reassemble → one stdout line per peer event.
# Canonical reference lives in ALL_DESIGN_IMPLEMENTATION/CLIENT.md §2.2.
# Env: BASE_URL, AGENT_ID, CID, STATE_FILE (opt).
import json, os, random, sys, time, requests
from collections import OrderedDict


def need(v):
    x = os.environ.get(v, "").strip()
    if not x:
        sys.exit(f"listen.py: missing {v}")
    return x


def emit(o):
    sys.stdout.write(json.dumps(o, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def load(p):
    try:
        return int(open(p).read().strip()) if p and os.path.exists(p) else None
    except Exception:
        return None


def save(p, seq):
    if not p:
        return
    try:
        open(p + ".tmp", "w").write(str(seq))
        os.replace(p + ".tmp", p)
    except Exception:
        pass


def handle(etype, d, seq, me, pending):
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
        if not e or e["sender"] == me:
            return
        out = {
            "kind": "message" if etype == "message_end" else "aborted",
            "seq": seq,
            "sender": e["sender"],
            "message_id": d["message_id"],
            "content": "".join(e["c"]),
            "timestamp": d.get("timestamp", ""),
        }
        if etype == "message_abort":
            out["reason"] = d.get("reason", "")
        emit(out)
        return
    if etype in ("agent_joined", "agent_left"):
        aid = d.get("agent_id", "")
        if aid and aid != me:
            emit({
                "kind": "joined" if etype == "agent_joined" else "left",
                "seq": seq,
                "agent_id": aid,
                "timestamp": d.get("timestamp", ""),
            })


def main():
    base = need("BASE_URL").rstrip("/")
    agent, cid = need("AGENT_ID"), need("CID")
    state = os.environ.get("STATE_FILE") or None
    last, seen, pending, attempt = load(state), OrderedDict(), {}, 0
    while True:
        try:
            h = {"X-Agent-ID": agent, "Accept": "text/event-stream"}
            if last is not None:
                h["Last-Event-ID"] = str(last)
            with requests.get(
                f"{base}/conversations/{cid}/stream",
                headers=h,
                stream=True,
                timeout=(10, None),
            ) as r:
                if r.status_code in (400, 403, 404):
                    sys.exit(f"listen.py: fatal {r.status_code}: {r.text[:500]}")
                r.raise_for_status()
                attempt = 0
                seq = etype = data = None
                for raw in r.iter_lines(decode_unicode=True):
                    if raw is None:
                        continue
                    if raw == "":
                        if etype and data is not None and seq is not None and seq not in seen:
                            seen[seq] = None
                            if len(seen) > 4096:
                                seen.popitem(last=False)
                            try:
                                handle(etype, json.loads(data), seq, agent, pending)
                            except ValueError:
                                pass
                            last = seq
                            save(state, last)
                        seq = etype = data = None
                        continue
                    if raw.startswith(":"):
                        continue
                    k, _, v = raw.partition(": ")
                    if k == "id":
                        try:
                            seq = int(v)
                        except ValueError:
                            seq = None
                    elif k == "event":
                        etype = v
                    elif k == "data":
                        data = v
        except requests.RequestException:
            pass
        attempt += 1
        time.sleep(min(30.0, 0.5 * 2 ** attempt) * random.random())


if __name__ == "__main__":
    main()
