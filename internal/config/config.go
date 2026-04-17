// Package config loads server configuration from environment variables.
//
// Every tunable lives here so main.go has a single place to fail-fast at
// startup. See ALL_DESIGN_IMPLEMENTATION/server-lifecycle-plan.md §Configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Config is the resolved configuration for a single server instance.
// Loaded once at startup via Load(); never mutated thereafter.
type Config struct {
	// --- Required ---
	DatabaseURL string // DATABASE_URL — Neon pooled DSN
	S2AuthToken string // S2_AUTH_TOKEN — S2 bearer token

	// --- Optional with defaults ---
	Port                uint16        // PORT (default 8080)
	LogLevel            string        // LOG_LEVEL (default "info")
	ShutdownTimeout     time.Duration // SHUTDOWN_TIMEOUT_SECONDS (default 30s)
	S2Basin             string        // S2_BASIN (default "agentmail")
	CursorFlushInterval time.Duration // CURSOR_FLUSH_INTERVAL_S (default 5s)
	HealthCheckTimeout  time.Duration // HEALTH_CHECK_TIMEOUT_S (default 5s)

	// --- Resident agent (optional; when set, AnthropicAPIKey becomes required) ---
	ResidentAgentID uuid.UUID // RESIDENT_AGENT_ID (default uuid.Nil)
	AnthropicAPIKey string    // ANTHROPIC_API_KEY (required iff ResidentAgentID != Nil)
}

// HasResidentAgent reports whether the resident Claude-powered agent is
// enabled. When true, the server must bootstrap the agent at startup.
func (c Config) HasResidentAgent() bool {
	return c.ResidentAgentID != uuid.Nil
}

// Load reads Config from the process environment, applies defaults, and
// validates required fields. Returns a non-nil error on any missing
// required variable or invalid value.
func Load() (Config, error) {
	c := Config{
		Port:                8080,
		LogLevel:            "info",
		ShutdownTimeout:     30 * time.Second,
		S2Basin:             "agentmail",
		CursorFlushInterval: 5 * time.Second,
		HealthCheckTimeout:  5 * time.Second,
	}

	// Required ---------------------------------------------------------
	c.DatabaseURL = os.Getenv("DATABASE_URL")
	if c.DatabaseURL == "" {
		return c, errors.New("config: DATABASE_URL is required")
	}
	c.S2AuthToken = os.Getenv("S2_AUTH_TOKEN")
	if c.S2AuthToken == "" {
		// Accept either S2_AUTH_TOKEN or S2_ACCESS_TOKEN for convenience.
		c.S2AuthToken = os.Getenv("S2_ACCESS_TOKEN")
	}
	if c.S2AuthToken == "" {
		return c, errors.New("config: S2_AUTH_TOKEN (or S2_ACCESS_TOKEN) is required")
	}

	// Optional ---------------------------------------------------------
	if v := os.Getenv("PORT"); v != "" {
		p, err := strconv.ParseUint(v, 10, 16)
		if err != nil || p == 0 {
			return c, fmt.Errorf("config: invalid PORT %q", v)
		}
		c.Port = uint16(p)
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		switch v {
		case "debug", "info", "warn", "error":
			c.LogLevel = v
		default:
			return c, fmt.Errorf("config: invalid LOG_LEVEL %q (want debug|info|warn|error)", v)
		}
	}
	if v := os.Getenv("SHUTDOWN_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 5 || n > 300 {
			return c, fmt.Errorf("config: invalid SHUTDOWN_TIMEOUT_SECONDS %q (want 5-300)", v)
		}
		c.ShutdownTimeout = time.Duration(n) * time.Second
	}
	if v := os.Getenv("S2_BASIN"); v != "" {
		c.S2Basin = v
	}
	if v := os.Getenv("CURSOR_FLUSH_INTERVAL_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 60 {
			return c, fmt.Errorf("config: invalid CURSOR_FLUSH_INTERVAL_S %q (want 1-60)", v)
		}
		c.CursorFlushInterval = time.Duration(n) * time.Second
	}
	if v := os.Getenv("HEALTH_CHECK_TIMEOUT_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 30 {
			return c, fmt.Errorf("config: invalid HEALTH_CHECK_TIMEOUT_S %q (want 1-30)", v)
		}
		c.HealthCheckTimeout = time.Duration(n) * time.Second
	}

	// Resident agent ---------------------------------------------------
	if v := os.Getenv("RESIDENT_AGENT_ID"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return c, fmt.Errorf("config: invalid RESIDENT_AGENT_ID %q: %w", v, err)
		}
		c.ResidentAgentID = id
	}
	c.AnthropicAPIKey = os.Getenv("ANTHROPIC_API_KEY")
	if c.HasResidentAgent() && c.AnthropicAPIKey == "" {
		return c, errors.New("config: ANTHROPIC_API_KEY is required when RESIDENT_AGENT_ID is set")
	}

	return c, nil
}
