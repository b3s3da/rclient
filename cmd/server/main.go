package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"rclient/internal/server"
)

// envOr returns env[key] if set, otherwise def.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg := server.Config{
		Listen:     envOr("RCLIENT_LISTEN", ":8080"),
		AgentPath:  os.Getenv("RCLIENT_AGENT_PATH"),
		PanelPath:  os.Getenv("RCLIENT_PANEL_PATH"),
		AgentToken: os.Getenv("RCLIENT_AGENT_TOKEN"),
		PanelUser:  envOr("RCLIENT_PANEL_USER", "admin"),
		PanelPass:  os.Getenv("RCLIENT_PANEL_PASS"),
		EnrollPath: envOr("RCLIENT_ENROLL_PATH", "/var/lib/rclient/enroll.json"),
	}
	if err := cfg.Validate(); err != nil {
		log.Error("bad config", "err", err)
		log.Error("required env vars: RCLIENT_AGENT_PATH, RCLIENT_PANEL_PATH, RCLIENT_AGENT_TOKEN, RCLIENT_PANEL_PASS")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(cfg, log)
	if err != nil {
		log.Error("server init failed", "err", err)
		os.Exit(1)
	}
	if err := srv.Run(ctx); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}
