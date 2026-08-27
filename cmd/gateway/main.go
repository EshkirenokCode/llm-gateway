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

	"github.com/EshkirenokCode/llm-gateway/internal/auth"
	"github.com/EshkirenokCode/llm-gateway/internal/billing"
	"github.com/EshkirenokCode/llm-gateway/internal/config"
	"github.com/EshkirenokCode/llm-gateway/internal/providers"
	"github.com/EshkirenokCode/llm-gateway/internal/proxy"
	"github.com/EshkirenokCode/llm-gateway/internal/storage"
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

	pricing := billing.DefaultPricing()
	if cfg.PricingFile != "" {
		pricing, err = billing.LoadPricing(cfg.PricingFile)
		if err != nil {
			return err
		}
		logger.Info("pricing loaded from file", "path", cfg.PricingFile)
	}

	opts := proxy.Options{
		Provider:       providers.NewMock(providers.MockOptions{}),
		Logger:         logger,
		Metrics:        telemetry.NewMetrics(),
		Pricing:        pricing,
		IdempotencyTTL: cfg.IdempotencyTTL,
	}

	checks := map[string]proxy.ReadinessCheck{}

	if cfg.PersistenceEnabled() {
		store, err := openStore(ctx, cfg, logger)
		if err != nil {
			return err
		}
		defer store.Close()

		limiter := billing.NewRateLimiter(cfg.RateLimitBurstRatio)

		opts.Store = store
		opts.Auth = auth.NewAuthenticator(store, cfg.KeyCacheTTL)
		opts.Budget = billing.NewBudget(store, cfg.BudgetCacheTTL)
		opts.Limiter = limiter

		checks["database"] = store.Ping

		go housekeeping(ctx, store, limiter, logger)
	} else {
		logger.Warn("running without a database: no tenants, accounting or budgets")
	}

	opts.Health = proxy.NewHealth(version, checks)
	handler := proxy.NewHandler(opts)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	// ListenAndServe blocks, so it runs in its own goroutine and reports a
	// startup failure back over a buffered channel.
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening",
			"addr", cfg.HTTPAddr,
			"version", version,
			"provider", opts.Provider.Name(),
			"persistence", cfg.PersistenceEnabled())
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
	opts.Health.BeginShutdown()

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

func openStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (*storage.Store, error) {
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	store, err := storage.Open(connectCtx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return nil, err
	}

	if cfg.MigrateOnStart {
		migrateCtx, cancelMigrate := context.WithTimeout(ctx, time.Minute)
		defer cancelMigrate()

		if err := storage.Migrate(migrateCtx, store.Pool); err != nil {
			store.Close()
			return nil, err
		}
		logger.Info("migrations applied")
	}

	return store, nil
}

// housekeeping drops rate limiter buckets and idempotency rows that nobody
// will look at again. Both grow without bound otherwise.
func housekeeping(ctx context.Context, store *storage.Store, limiter *billing.RateLimiter, logger *slog.Logger) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			limiter.Sweep(time.Hour)

			removed, err := store.DeleteExpiredIdempotencyKeys(ctx)
			if err != nil {
				logger.Warn("could not clean up idempotency keys", "error", err)
				continue
			}
			if removed > 0 {
				logger.Info("expired idempotency keys removed", "count", removed)
			}
		}
	}
}
