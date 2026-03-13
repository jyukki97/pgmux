package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jyukki97/pgmux/internal/admin"
	"github.com/jyukki97/pgmux/internal/cache"
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
	pprofAddr := ""
	for _, arg := range os.Args[1:] {
		if arg == "-debug" {
			debug = true
		} else if arg == "-pprof" {
			pprofAddr = "localhost:6060"
		} else {
			cfgPath = arg
		}
	}

	if debug {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	// Start pprof server if requested
	if pprofAddr != "" {
		go func() {
			slog.Info("pprof server starting", "listen", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				slog.Error("pprof server", "error", err)
			}
		}()
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

	// Collect HTTP servers for graceful shutdown
	var httpServers []*http.Server

	// Channel to propagate HTTP bind errors to main goroutine
	httpErrCh := make(chan error, 3)

	// Start Prometheus metrics HTTP server
	if cfg.Metrics.Enabled {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		metricsSrv := &http.Server{Handler: mux}
		httpServers = append(httpServers, metricsSrv)

		ln, err := net.Listen("tcp", cfg.Metrics.Listen)
		if err != nil {
			return fmt.Errorf("metrics server bind %s: %w", cfg.Metrics.Listen, err)
		}
		slog.Info("metrics server starting", "listen", cfg.Metrics.Listen)
		go func() {
			if err := metricsSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
				httpErrCh <- fmt.Errorf("metrics server: %w", err)
			}
		}()
	}

	srv := proxy.NewServer(cfg)

	// Start Admin API server
	if cfg.Admin.Enabled {
		adminSrv := admin.New(srv.Cfg, srv.Cache, srv.Invalidator, srv.DBGroups, srv.DefaultDBName(), srv.AuditLogger, func() any {
			m := srv.QueryMirror()
			if m == nil {
				return nil
			}
			return map[string]any{
				"queries": m.Stats(),
				"sent":    m.Sent(),
				"dropped": m.Dropped(),
				"errors":  m.Errors(),
			}
		}, func() any {
			d := srv.QueryDigest()
			if d == nil {
				return nil
			}
			return d.TopN(100)
		}, func() {
			d := srv.QueryDigest()
			if d != nil {
				d.Reset()
			}
		}, func() any {
			ct := srv.ConnTracker()
			if ct == nil {
				return nil
			}
			return ct.Stats()
		})
		adminSrv.SetReloadFunc(func() error {
			return reloadConfig(cfgPath, srv)
		})
		adminHTTP := adminSrv.HTTPServer()

		ln, err := net.Listen("tcp", cfg.Admin.Listen)
		if err != nil {
			return fmt.Errorf("admin server bind %s: %w", cfg.Admin.Listen, err)
		}
		httpServers = append(httpServers, adminHTTP)
		slog.Info("admin server starting", "listen", cfg.Admin.Listen)
		go func() {
			if err := adminHTTP.Serve(ln); err != nil && err != http.ErrServerClosed {
				httpErrCh <- fmt.Errorf("admin server: %w", err)
			}
		}()
	}

	// Start Data API server
	if cfg.DataAPI.Enabled {
		apiSrv := dataapi.New(srv.Cfg, srv.DBGroups, srv.DefaultDBName(), srv.Cache, srv.ProxyMetrics(), srv.RateLimiter, func() *cache.Invalidator { return srv.Invalidator() })
		apiHTTP := apiSrv.HTTPServer()

		ln, err := net.Listen("tcp", cfg.DataAPI.Listen)
		if err != nil {
			return fmt.Errorf("data api server bind %s: %w", cfg.DataAPI.Listen, err)
		}
		httpServers = append(httpServers, apiHTTP)
		slog.Info("data api server starting", "listen", cfg.DataAPI.Listen)
		go func() {
			if err := apiHTTP.Serve(ln); err != nil && err != http.ErrServerClosed {
				httpErrCh <- fmt.Errorf("data api server: %w", err)
			}
		}()
	}

	// Watch for HTTP server runtime errors
	go func() {
		if err := <-httpErrCh; err != nil {
			slog.Error("http server runtime error", "error", err)
			cancel()
		}
	}()

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
		<-fw.Ready() // wait for watcher to be armed before proceeding
	}

	// Graceful shutdown of HTTP servers when context is cancelled
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		for _, s := range httpServers {
			if err := s.Shutdown(shutdownCtx); err != nil {
				slog.Error("http server shutdown", "error", err)
			}
		}
	}()

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
