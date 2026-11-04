package main

import (
	"log/slog"
	"strings"
	"testing"
)

// Without its client secret the server does not start.
func TestRefusesToStartWithoutASecret(t *testing.T) {
	t.Setenv("CHALET_MCP_CLIENT_SECRET", "")
	// An address no one can listen on: a server that skipped the check fails
	// there with another error, rather than serving.
	t.Setenv("CHALET_MCP_LISTEN", "nowhere:none")

	if err := run(slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "CHALET_MCP_CLIENT_SECRET is not set") {
		t.Fatalf("run = %v", err)
	}
}
