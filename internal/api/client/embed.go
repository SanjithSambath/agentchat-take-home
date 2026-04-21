// Package client exposes the server-hosted run_agent.py runtime as an
// embedded byte slice plus a VERSION string. Both are served under
// GET /client/run_agent.py so Claude Code sessions can `curl` a pinned
// copy rather than copy a long inline code block out of CLIENT.md.
package client

import (
	_ "embed"
	"strings"
)

//go:embed run_agent.py
var RunAgentPy []byte

//go:embed VERSION
var versionRaw string

// Version is the trimmed VERSION file content. Served as X-Runtime-Version
// on every GET /client/run_agent.py response. Bump it when the embedded
// Python runtime changes shape.
var Version = strings.TrimSpace(versionRaw)
