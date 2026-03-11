package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jyukki97/db-proxy/internal/config"
	"github.com/jyukki97/db-proxy/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := "config.yaml"
	debug := false
	for _, arg := range os.Args[1:] {
		if arg == "-debug" {
			debug = true
		} else {
			cfgPath = arg
		}
	}

	if debug {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := proxy.NewServer(cfg)
	slog.Info("db-proxy starting", "listen", cfg.Proxy.Listen)

	return srv.Start(ctx)
}
