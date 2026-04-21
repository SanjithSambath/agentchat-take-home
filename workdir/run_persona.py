#!/usr/bin/env python3
# run_persona.py — single-command persona loop: listen + speak + stop + leave.
import argparse, json, os, queue, random, signal, sys, threading, time
from pathlib import Path
import requests
from anthropic import Anthropic

def uuid7():
    ts = int(time.time() * 1000).to_bytes(6, "big")
    r = bytearray(os.urandom(10))
    r[0] = (r[0] & 0x0F) | 0x70; r[2] = (r[2] & 0x3F) | 0x80
    h = (ts + bytes(r)).hex()
    return f"{h[0:8]}-{h[8:12]}-{h[12:16]}-{h[16:20]}-{h[20:32]}"

def need(v):
    x = os.environ.get(v, "").strip()
    if not x: sys.exit(f"run_persona.py: missing env {v}")
    return x

def state_dir():
    home = Path.home() / ".agentmail"
    try: home.mkdir(parents=True, exist_ok=True); return home
    except Exception:
        print("run_persona.py: ~/.agentmail/ not writable, falling back to /tmp", file=sys.stderr)
        return Path("/tmp")

def _load(p):
    try: return p.read_text().strip() if p.exists() else None
    except Exception: return None

def _save(p, v):
    try:
        tmp = p.with_suffix(p.suffix + ".tmp")
        tmp.write_text(v); tmp.replace(p)
    except Exception as e:
        print(f"run_persona.py: could not persist {p}: {e}", file=sys.stderr)

def http_json(method, url, agent=None, **kw):
    h = kw.pop("headers", {})
    if agent: h["X-Agent-ID"] = agent
    r = requests.request(method, url, headers=h, timeout=30, **kw)
    r.raise_for_status()
    return r.json() if r.content else {}

def register_or_resume(base, id_path):
    aid = _load(id_path)
    if aid: return aid
    aid = http_json("POST", f"{base}/agents")["agent_id"]
    _save(id_path, aid); return aid

def resolve_conversation(base, agent, cid_path, cid_arg, peer):
    if cid_arg: return cid_arg
    prior = _load(cid_path)
    if prior:
        try:
            members = http_json("GET", f"{base}/conversations/{prior}", agent).get("members", [])
            if agent in members: return prior
        except Exception: pass
    cid = http_json("POST", f"{base}/conversations", agent)["conversation_id"]
    _save(cid_path, cid)
    if peer:
        http_json("POST", f"{base}/conversations/{cid}/invite", agent,
                  json={"agent_id": peer})
    return cid

def fetch_history(base, agent, cid, limit=100):
    r = http_json("GET", f"{base}/conversations/{cid}/messages", agent,
                  params={"limit": limit})
    return list(reversed(r.get("messages", []) or []))

def leave(base, agent, cid):
    try: requests.post(f"{base}/conversations/{cid}/leave",
                       headers={"X-Agent-ID": agent}, timeout=10)
    except Exception: pass

class Tail(threading.Thread):
    def __init__(self, base, agent, cid, state_file, out_q, stop_evt):
        super().__init__(daemon=True)
        self.base, self.agent, self.cid = base, agent, cid
        self.state, self.out, self.stop = state_file, out_q, stop_evt
        self.armed = threading.Event()
        self.last = self._load_last()

    def _load_last(self):
        try: return int(self.state.read_text().strip()) if self.state.exists() else None
        except Exception: return None

    def _save_last(self, seq):
        try:
            tmp = self.state.with_suffix(self.state.suffix + ".tmp")
            tmp.write_text(str(seq)); tmp.replace(self.state)
        except Exception: pass

    def run(self):
        seen, pending, attempt = {}, {}, 0
        while not self.stop.is_set():
            try:
                h = {"X-Agent-ID": self.agent, "Accept": "text/event-stream"}
                if self.last is not None: h["Last-Event-ID"] = str(self.last)
                with requests.get(f"{self.base}/conversations/{self.cid}/stream",
                                  headers=h, stream=True, timeout=(10, None)) as r:
                    if r.status_code in (400, 403, 404):
                        self.out.put({"kind": "fatal", "msg": f"{r.status_code}: {r.text[:200]}"})
                        return
                    r.raise_for_status()
                    attempt = 0; self.armed.set()
                    seq = etype = data = None
                    for raw in r.iter_lines(decode_unicode=True):
                        if self.stop.is_set(): return
                        if raw is None: continue
                        if raw == "":
                            if etype and data is not None and seq is not None and seq not in seen:
                                seen[seq] = True
                                if len(seen) > 4096: seen.pop(next(iter(seen)))
                                try: self._emit(etype, json.loads(data), seq, pending)
                                except ValueError: pass
                                self.last = seq; self._save_last(seq)
                            seq = etype = data = None; continue
                        if raw.startswith(":"): self.armed.set(); continue
                        k, _, v = raw.partition(": ")
                        if k == "id":
                            try: seq = int(v)
                            except ValueError: seq = None
                        elif k == "event": etype = v
                        elif k == "data": data = v
            except requests.RequestException: pass
            attempt += 1
            time.sleep(min(30.0, 0.5 * 2 ** attempt) * random.random())

    def _emit(self, etype, d, seq, pending):
        if etype == "message_start":
            pending[d["message_id"]] = {"sender": d.get("sender_id", ""), "c": []}
        elif etype == "message_append":
            e = pending.get(d["message_id"])
            if e: e["c"].append(d.get("content", ""))
        elif etype in ("message_end", "message_abort"):
            e = pending.pop(d["message_id"], None)
            if not e or e["sender"] == self.agent: return
            kind = "message" if etype == "message_end" else "aborted"
            self.out.put({"kind": kind, "seq": seq, "sender": e["sender"],
                          "content": "".join(e["c"])})
        elif etype in ("agent_joined", "agent_left"):
            aid = d.get("agent_id", "")
            if aid and aid != self.agent:
                self.out.put({"kind": "joined" if etype == "agent_joined" else "left",
                              "agent_id": aid})

def _to_messages(hist, me):
    out = []
    for m in hist:
        c = (m.get("content") or "").strip()
        if not c: continue
        role = "assistant" if m.get("sender_id") == me else "user"
        if out and out[-1]["role"] == role: out[-1]["content"] += "\n\n" + c
        else: out.append({"role": role, "content": c})
    while out and out[0]["role"] != "user": out.pop(0)
    return out

def speak(base, agent, cid, persona, model, max_tokens, directive=None):
    msgs = _to_messages(fetch_history(base, agent, cid), agent)
    if directive:
        tag = "[Operator directive] " + directive
        if msgs and msgs[-1]["role"] == "user": msgs[-1]["content"] += "\n\n" + tag
        else: msgs.append({"role": "user", "content": tag})
    if not msgs: msgs = [{"role": "user", "content": "begin."}]

    mid = uuid7()
    for attempt in range(6):
        sent = []
        def body():
            yield (json.dumps({"message_id": mid}) + "\n").encode()
            with Anthropic().messages.stream(model=model, max_tokens=max_tokens,
                                             system=persona, messages=msgs) as s:
                for ev in s:
                    if ev.type == "content_block_delta" and getattr(ev.delta, "text", None):
                        sent.append(ev.delta.text)
                        yield (json.dumps({"content": ev.delta.text}) + "\n").encode()
        try:
            r = requests.post(f"{base}/conversations/{cid}/messages/stream",
                              headers={"X-Agent-ID": agent,
                                       "Content-Type": "application/x-ndjson"},
                              data=body(), timeout=(10, 300))
        except requests.RequestException:
            time.sleep(min(30.0, 0.5 * 2 ** (attempt + 1)) * random.random()); continue
        if r.ok: return "".join(sent)
        code = ""
        try: code = (r.json() or {}).get("error", {}).get("code", "")
        except Exception: pass
        if code == "in_progress_conflict":
            time.sleep(0.3 + random.random() * 0.7); continue
        if code == "already_aborted":
            mid = uuid7(); continue
        if code in ("slow_writer", "idle_timeout", "max_duration"):
            mid = uuid7(); time.sleep(min(10.0, 0.5 * 2 ** (attempt + 1)) * random.random()); continue
        if r.status_code >= 500 or code in ("internal_error", "timeout"):
            time.sleep(min(30.0, 0.5 * 2 ** (attempt + 1)) * random.random()); continue
        sys.exit(f"run_persona.py: speak failed {r.status_code}: {r.text[:300]}")
    sys.exit("run_persona.py: speak retries exhausted")

_FAREWELL = ("appreciate", "appreciated", "thanks", "thank you", "good talk",
             "wrapping up", "that's it", "signing off", "see you")

def stop_when_silent(my_last):
    if not my_last: return False
    if len(my_last.split()) >= 40: return False
    if "?" in my_last: return False
    low = my_last.lower()
    return any(tok in low for tok in _FAREWELL)

def should_respond(policy, ev, me, persona_name, recent):
    if ev.get("kind") not in ("message", "aborted"): return False
    if policy == "always": return True
    if policy == "address-me":
        if not persona_name: return True
        return persona_name.lower() in ev["content"].lower()
    if policy == "round-robin":
        tail = recent[-2:]
        return not (tail and all(s == me for s in tail))
    return True

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--persona", required=True)
    ap.add_argument("--persona-name", default=None,
                    help="name used by --policy address-me")
    ap.add_argument("--peer", default=None)
    ap.add_argument("--resident", action="store_true")
    ap.add_argument("--conversation", default=None)
    ap.add_argument("--policy", choices=["always", "address-me", "round-robin"],
                    default="always")
    ap.add_argument("--max-turns", type=int, default=10)
    ap.add_argument("--max-tokens", type=int, default=1024)
    ap.add_argument("--model", default=os.environ.get("MODEL", "claude-sonnet-4-5"))
    ap.add_argument("--opener", default=None,
                    help="operator directive; used only if conversation has no prior content")
    ap.add_argument("--no-stop-heuristic", action="store_true")
    args = ap.parse_args()

    base = need("BASE_URL").rstrip("/"); need("ANTHROPIC_API_KEY")
    persona = open(args.persona, encoding="utf-8").read().strip()
    sd = state_dir()
    agent = register_or_resume(base, sd / "agent.id")

    peer = None
    if args.resident:
        try: peer = http_json("GET", f"{base}/agents/resident")["agent_id"]
        except Exception as e: sys.exit(f"run_persona.py: resident unavailable: {e}")
    elif args.peer: peer = args.peer

    cid = resolve_conversation(base, agent, sd / "conv.id", args.conversation, peer)

    events, stop_evt = queue.Queue(), threading.Event()
    tail = Tail(base, agent, cid, sd / f"listen_{agent}_{cid}.state", events, stop_evt)
    tail.start()
    if not tail.armed.wait(timeout=15):
        sys.exit("run_persona.py: SSE tail failed to arm within 15s")

    recent, turns, closing = [], 0, False
    def shutdown(*_):
        nonlocal closing; closing = True
    signal.signal(signal.SIGINT, shutdown)
    signal.signal(signal.SIGTERM, shutdown)

    history_empty = len(fetch_history(base, agent, cid)) == 0
    if history_empty and args.opener:
        my_text = speak(base, agent, cid, persona, args.model, args.max_tokens,
                        directive=args.opener)
        recent.append(agent); turns += 1
        if not args.no_stop_heuristic and stop_when_silent(my_text): closing = True

    while not closing and turns < args.max_turns:
        try: ev = events.get(timeout=1.0)
        except queue.Empty: continue
        if ev.get("kind") == "fatal":
            print(f"run_persona.py: SSE fatal — {ev.get('msg')}", file=sys.stderr); break
        if ev.get("kind") in ("joined", "left"): continue
        if ev.get("kind") not in ("message", "aborted"): continue
        recent.append(ev["sender"])
        if not should_respond(args.policy, ev, agent, args.persona_name, recent): continue
        my_text = speak(base, agent, cid, persona, args.model, args.max_tokens)
        recent.append(agent); turns += 1
        if not args.no_stop_heuristic and stop_when_silent(my_text): break

    stop_evt.set()
    leave(base, agent, cid)
    print(json.dumps({"status": "done", "turns": turns,
                      "conversation_id": cid, "agent_id": agent}))

if __name__ == "__main__": main()
