package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jyukki97/pgmux/internal/admin"
	"github.com/jyukki97/pgmux/internal/config"
	"github.com/jyukki97/pgmux/internal/dataapi"
	"github.com/jyukki97/pgmux/internal/proxy"
	"github.com/jyukki97/pgmux/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

	// Initialize OpenTelemetry
	otelShutdown, err := telemetry.Init(cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			slog.Error("telemetry shutdown", "error", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start Prometheus metrics HTTP server
	if cfg.Metrics.Enabled {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		go func() {
			slog.Info("metrics server starting", "listen", cfg.Metrics.Listen)
			if err := http.ListenAndServe(cfg.Metrics.Listen, mux); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server error", "error", err)
			}
		}()
	}

	srv := proxy.NewServer(cfg)

	// Start Admin API server
	if cfg.Admin.Enabled {
		adminSrv := admin.New(srv.Cfg, srv.Cache, srv.Invalidator, srv.WriterPool, srv.ReaderPools, srv.AuditLogger)
		adminSrv.SetReloadFunc(func() error {
			return reloadConfig(cfgPath, srv)
		})
		go func() {
			if err := adminSrv.ListenAndServe(cfg.Admin.Listen); err != nil && err != http.ErrServerClosed {
				slog.Error("admin server error", "error", err)
			}
		}()
	}

	// Start Data API server
	if cfg.DataAPI.Enabled {
		apiSrv := dataapi.New(srv.Cfg, srv.WriterPool, srv.ReaderPools, srv.Balancer, srv.Cache, srv.ProxyMetrics(), srv.RateLimiter)
		go func() {
			if err := apiSrv.ListenAndServe(cfg.DataAPI.Listen); err != nil && err != http.ErrServerClosed {
				slog.Error("data api server error", "error", err)
			}
		}()
	}

	// Handle SIGHUP for config reload
	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)
	go func() {
		for range sighupCh {
			slog.Info("received SIGHUP, reloading config...")
			if err := reloadConfig(cfgPath, srv); err != nil {
				slog.Error("config reload failed", "error", err)
			}
		}
	}()

	// Start config file watcher if enabled
	if cfg.ConfigOptions.Watch {
		fw, err := config.NewFileWatcher(cfgPath, func() {
			slog.Info("config file changed, reloading", "path", cfgPath)
			if err := reloadConfig(cfgPath, srv); err != nil {
				slog.Error("config reload failed", "error", err)
			}
		})
		if err != nil {
			return fmt.Errorf("create config file watcher: %w", err)
		}
		defer fw.Stop()
		go func() {
			if err := fw.Start(ctx); err != nil {
				slog.Error("config file watcher error", "error", err)
			}
		}()
	}

	slog.Info("pgmux starting", "listen", cfg.Proxy.Listen)

	return srv.Start(ctx)
}

func reloadConfig(cfgPath string, srv *proxy.Server) error {
	newCfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return srv.Reload(newCfg)
}
