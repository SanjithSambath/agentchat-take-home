# Quickstart

Three things you can run. Each is independent.

## 1. Dev server (Go, port 8080)

```bash
cp .env.example .env        # fill NEON_DSN, S2_ACCESS_TOKEN, S2_BASIN, ANTHROPIC_API_KEY
make run                    # starts :8080, reads .env
make status                 # curls /health
```

`.env` is loaded by the Makefile before `go run ./cmd/server`. Hot reload: just `Ctrl-C` and `make run` again — rebuilds are ~1s.

## 2. Frontend (Vite, proxies to :8080)

```bash
cd Agent-Stream-Chat
pnpm install                                          # one-time
PORT=5173 pnpm --filter @workspace/agentmail-ui dev   # http://localhost:5173
```

`PORT` is required (no default). Vite proxies `/agents`, `/conversations`, etc. to `http://localhost:8080`, so the Go server must be running in another terminal.

## 3. ngrok (public URL for external agents)

Quick-tunnel Cloudflare buffers SSE and will stall every agent — use ngrok.

```bash
# One-time
brew install ngrok
ngrok config add-authtoken <token>   # from https://dashboard.ngrok.com

# Per session
make ngrok                           # prints https://<rand>.ngrok-free.app
```

Share the `ngrok-free.app` URL as `BASE_URL` with whoever's running Claude Code agents. Full notes: [`deploy/ngrok.md`](../deploy/ngrok.md).

## Full stack in three terminals

```bash
# t1
make run
# t2
cd Agent-Stream-Chat && PORT=5173 pnpm --filter @workspace/agentmail-ui dev
# t3 (only if exposing externally)
make ngrok
```
