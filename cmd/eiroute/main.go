package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/engel75/eiroute/internal/backends"
	"github.com/engel75/eiroute/internal/config"
	errtpl "github.com/engel75/eiroute/internal/errors"
	"github.com/engel75/eiroute/internal/metrics"
	"github.com/engel75/eiroute/internal/models"
	"github.com/engel75/eiroute/internal/router"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "./config/config.yaml", "path to config file")
	flag.Parse()

	// Handle version subcommand
	if len(flag.Args()) > 0 && flag.Args()[0] == "version" {
		fmt.Println(version)
		return
	}

	// Load config first to determine log level.
	cfg, err := config.Load(*configPath)
	if err != nil {
		// Fallback logger before config is loaded.
		fallbackLog := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
		fallbackLog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: config.GetLogLevel(cfg)}))

	errTemplates, err := errtpl.LoadTemplates(cfg.ErrorTemplates)
	if err != nil {
		logger.Error("failed to load error templates", "error", err)
		os.Exit(1)
	}

	metrics.Register()

	pool, err := backends.NewPool(cfg.Backends)
	if err != nil {
		logger.Error("failed to create backend pool", "error", err)
		os.Exit(1)
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       cfg.IdleConnTimeout.Duration,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}

	rt := router.New(pool, errTemplates, transport, cfg.SemaphoreTimeout.Duration, logger)
	agg := models.NewAggregator(pool, cfg.OwnedByOverride)

	if config.GetLogLevel(cfg) == slog.LevelDebug {
		debugCtx, debugCancel := context.WithCancel(context.Background())
		defer debugCancel()
		go rt.StartDebugLogger(debugCtx)
	}

	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /v1/completions", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /v1/responses", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /v1/embeddings", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /v1/classify", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /v1/tokenize", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /v1/detokenize", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /v1/audio/transcriptions", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /v1/score", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /v1/rerank", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /v1/messages", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /api/chat", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("POST /api/generate", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))
	mux.Handle("GET /v1/models", router.RequestIDMiddleware(agg))
	mux.HandleFunc("GET /health", rt.HandleHealth)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("POST /-/reload", func(w http.ResponseWriter, r *http.Request) {
		newCfg, err := config.Reload(*configPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("reload failed: %v", err), http.StatusInternalServerError)
			return
		}
		if err := rt.ReloadErrors(newCfg.ErrorTemplates); err != nil {
			http.Error(w, fmt.Sprintf("reload failed: %v", err), http.StatusInternalServerError)
			return
		}
		if err := pool.ReloadPool(newCfg.Backends); err != nil {
			http.Error(w, fmt.Sprintf("reload failed: %v", err), http.StatusInternalServerError)
			return
		}
		agg.Reload()
		logger.Info("config reloaded via HTTP", "backends", len(newCfg.Backends))
		w.Write([]byte("OK\n"))
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       cfg.IdleConnTimeout.Duration,
	}

	healthCtx, healthCancel := context.WithCancel(context.Background())
	defer healthCancel()
	backends.StartHealthChecks(healthCtx, pool.Backends(), logger)

	reload := func() {
		logger.Debug("reload: reading config", "path", *configPath)
		newCfg, err := config.Reload(*configPath)
		if err != nil {
			logger.Error("config reload failed", "error", err)
			return
		}
		logger.Debug("reload: updating error templates", "path", newCfg.ErrorTemplates)
		if err := rt.ReloadErrors(newCfg.ErrorTemplates); err != nil {
			logger.Error("error template reload failed", "error", err)
			return
		}
		logger.Debug("reload: updating backend pool", "backends", len(newCfg.Backends))
		if err := pool.ReloadPool(newCfg.Backends); err != nil {
			logger.Error("backend pool reload failed", "error", err)
			return
		}
		agg.Reload()
		logger.Info("config reloaded", "backends", len(newCfg.Backends))
	}

	go func() {
		logger.Info("starting eiroute", "listen", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for {
		sig := <-sigCh
		if sig == syscall.SIGHUP {
			logger.Info("received SIGHUP, reloading config")
			reload()
			continue
		}
		logger.Info("received signal, shutting down", "signal", sig)
		break
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer shutdownCancel()

	healthCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
		os.Exit(1)
	}

	logger.Info("shutdown complete")
}
