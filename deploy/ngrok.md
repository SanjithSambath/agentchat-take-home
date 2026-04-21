# ngrok — public SSE-capable transport for AgentMail

This is the recommended transport when you need an external evaluator (e.g.
Haakam) to connect their own Claude Code agents to your local AgentMail
server. It's a free-tier alternative to a named Cloudflare tunnel that
doesn't require owning a domain.

## Why not `make tunnel` (Cloudflare quick tunnel)?

`cloudflared tunnel --url http://localhost:8080` creates an **ephemeral quick
tunnel** at `*.trycloudflare.com`. Quick tunnels **do not honor the
`disableChunkedEncoding: false` flag** that named tunnels respect, so they
buffer small HTTP chunks unpredictably. SSE events (~100 bytes each) and
heartbeats (~14 bytes) get coalesced on the tunnel's edge. Measured: a
local SSE stream that delivers `:ok\n\n` plus the first event in ~500 ms
delivered **0 bytes over 35 s** through the quick tunnel. That stalls every
`run_agent.py` daemon that connects through it.

ngrok's free tier uses HTTP/2 to the client with no default buffering for
streaming responses, so SSE works out of the box.

## One-time setup

```bash
# 1. Install (macOS)
brew install ngrok

# 2. Sign up for a free account (no card required): https://dashboard.ngrok.com/signup
# 3. Copy your authtoken from https://dashboard.ngrok.com/get-started/your-authtoken
ngrok config add-authtoken <your-token>
```

That's it. The authtoken lives in `~/Library/Application Support/ngrok/ngrok.yml`.

## Per-session usage

```bash
# Terminal 1: start the AgentMail server
make run

# Terminal 2: open the tunnel
make ngrok
```

`make ngrok` prints a line like:

```
started tunnel ... url=https://a1b2c3d4e5f6.ngrok-free.app
```

Copy that URL. That's the `BASE_URL` you share with whoever is running
Claude Code (including yourself if you're testing from another machine).

```bash
# Paste into whatever shell / CC session is going to run the agent:
export BASE_URL=https://a1b2c3d4e5f6.ngrok-free.app
export ANTHROPIC_API_KEY=sk-ant-...
```

## Free-tier caveats

- **URL changes per session.** Every `make ngrok` restart gets a new random
  subdomain. If your evaluator is running long-lived tests, keep the
  terminal up. If you restart, share the new URL.
- **Rate limit: ~40 inbound requests per minute** (free tier). Irrelevant
  for SSE (one long-lived GET per conversation) and comfortable for
  NDJSON streaming writes (one POST per voice turn). Becomes relevant
  only if dozens of concurrent agents are hammering CRUD endpoints.
- **One concurrent tunnel per free account.** Run one ngrok per machine.

Upgrade to the paid plan ($10–20 / month) if you need a persistent custom
subdomain or want to run multiple tunnels. For a take-home demo the free
tier is sufficient.

## Alternative: named Cloudflare tunnel

If you own a domain already hosted on Cloudflare, a named tunnel is the
production path and also supports SSE correctly. See
`deploy/cloudflared.example.yml`. That's a ~5-min setup (login → create
tunnel → route DNS) and gives you a stable custom hostname.

## Troubleshooting

- **`ngrok: command not found`** — `brew install ngrok` or download from
  https://ngrok.com/download.
- **`authentication failed` when starting** — your authtoken isn't
  configured. Re-run `ngrok config add-authtoken <token>`.
- **SSE still stalls through ngrok** — very unlikely. Test localhost
  first with `curl -N http://localhost:8080/conversations/<cid>/stream
  -H "X-Agent-ID: <id>"` to confirm the server side is healthy, then
  repeat through the ngrok URL. If localhost works but ngrok doesn't,
  check ngrok's inspector UI (http://127.0.0.1:4040) for the raw
  traffic; if bytes are leaving the server but not arriving at ngrok's
  edge, it's a local firewall issue.
