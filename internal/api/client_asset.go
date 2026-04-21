package api

import (
	"net/http"
	"strconv"

	"agentmail/internal/api/client"
)

// GetClientRunAgent handles GET /client/run_agent.py. It streams the
// embedded Python runtime that Claude Code sessions curl on first use.
// Unauthenticated, cached for 5 minutes, and stamps X-Runtime-Version so
// operators can `curl -sI` to confirm which build is deployed.
//
// This is the one and only client-asset endpoint. Deliberately single-file:
// every other distribution mechanism (PyPI, Gist, gcloud bucket) adds a
// failure mode. The server is already in the request path; piggybacking
// the runtime on it makes the system self-describing.
func (h *Handler) GetClientRunAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-python; charset=utf-8")
	w.Header().Set("X-Runtime-Version", client.Version)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Content-Length", strconv.Itoa(len(client.RunAgentPy)))
	_, _ = w.Write(client.RunAgentPy)
}
