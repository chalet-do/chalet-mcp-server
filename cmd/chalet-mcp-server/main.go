// chalet-mcp-server serves Chalet's tools over HTTP to the clients that
// connect by URL and sign in with OAuth. It is configured by the environment
// alone (README): where Chalet runs, this server's URL, where it listens, and
// its own client credential for the token exchange.
package main

import (
	"cmp"
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chalet-do/chalet-mcp-server/internal/hosted"
)

// version is set at build time; `go build` leaves it at dev.
var version = "dev"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg := hosted.Config{
		Upstream:     cmp.Or(os.Getenv("CHALET_URL"), "https://chalet.do"),
		Resource:     cmp.Or(os.Getenv("CHALET_MCP_RESOURCE"), "https://mcp.chalet.do/mcp"),
		ClientID:     cmp.Or(os.Getenv("CHALET_MCP_CLIENT_ID"), "chalet-mcp-server"),
		ClientSecret: os.Getenv("CHALET_MCP_CLIENT_SECRET"),
	}
	handler, err := hosted.New(cfg, version, logger)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cmp.Or(os.Getenv("CHALET_MCP_LISTEN"), ":8080"))
	if err != nil {
		return err
	}
	if cfg.ClientSecret == "" {
		logger.Warn("CHALET_MCP_CLIENT_SECRET is not set: every request gets the 401")
	}
	logger.Info("serving Chalet over HTTP", "version", version, "listen", listener.Addr().String(), "url", cfg.Upstream, "resource", cfg.Resource, "client", cfg.ClientID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	// SIGTERM: what is in flight finishes, for up to 30 seconds.
	logger.Info("draining")
	drain, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return server.Shutdown(drain)
}
