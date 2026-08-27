// Command gateway is a self-hosted, OpenAI-compatible LLM gateway that adds
// per-tenant authentication, cost accounting, budgets and provider failover
// in front of multiple model providers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/EshkirenokCode/llm-gateway/internal/config"
	"github.com/EshkirenokCode/llm-gateway/internal/providers"
	"github.com/EshkirenokCode/llm-gateway/internal/proxy"
	"github.com/EshkirenokCode/llm-gateway/internal/telemetry"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	// The runtime image has no shell, so the container health check calls the
	// binary itself instead of curl.
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit")
	flag.Parse()

	if *healthcheck {
		if err := probe(); err != nil {
			fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		// The logger may not exist yet when configuration fails, so report the
		// fatal error on stderr and exit non-zero.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

// probe performs the container health check against the local server.
func probe() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	addr := cfg.HTTPAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	return nil
}

// run wires the dependencies together and blocks until the process is asked to
// terminate. Keeping it separate from main means every failure path can return
// an error instead of calling os.Exit, which would skip deferred cleanup.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	level, err := cfg.SlogLevel()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	// NotifyContext cancels ctx on the first SIGINT/SIGTERM and restores the
	// default handler afterwards, so a second signal kills the process outright.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	provider := providers.NewMock(providers.MockOptions{})
	metrics := telemetry.NewMetrics()
	health := proxy.NewHealth(version, nil)
	handler := proxy.NewHandler(provider, logger, metrics, health)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	// ListenAndServe blocks, so it runs in its own goroutine and reports a
	// startup failure back over a buffered channel.
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr, "version", version, "provider", provider.Name())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received", "timeout", cfg.ShutdownTimeout.String())
	}

	// Fail readiness first so a load balancer stops sending new requests while
	// the in-flight ones drain.
	health.BeginShutdown()

	// ctx is already cancelled here, so the shutdown deadline needs a fresh
	// parent context of its own.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	logger.Info("shutdown complete")

	return nil
}
