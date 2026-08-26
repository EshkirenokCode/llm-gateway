// Package config loads and validates the gateway configuration from the
// environment. Configuration is read once at startup; an invalid value is a
// fatal error, so the process never runs in a half-configured state.
package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config holds every runtime setting of the gateway.
type Config struct {
	// HTTPAddr is the address the public HTTP server listens on.
	HTTPAddr string `env:"GATEWAY_HTTP_ADDR" envDefault:":8080"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `env:"GATEWAY_LOG_LEVEL" envDefault:"info"`
	// ReadHeaderTimeout bounds how long a client may take to send request
	// headers. It is the cheapest defence against Slowloris-style attacks.
	ReadHeaderTimeout time.Duration `env:"GATEWAY_READ_HEADER_TIMEOUT" envDefault:"10s"`
	// ShutdownTimeout bounds how long in-flight requests may finish after a
	// termination signal before the server is closed forcefully.
	ShutdownTimeout time.Duration `env:"GATEWAY_SHUTDOWN_TIMEOUT" envDefault:"15s"`
}

// Load reads the configuration from the process environment and validates it.
func Load() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parse environment: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

// Validate reports the first invalid setting, if any.
func (c Config) Validate() error {
	if strings.TrimSpace(c.HTTPAddr) == "" {
		return fmt.Errorf("GATEWAY_HTTP_ADDR must not be empty")
	}

	if _, err := c.SlogLevel(); err != nil {
		return err
	}

	if c.ReadHeaderTimeout <= 0 {
		return fmt.Errorf("GATEWAY_READ_HEADER_TIMEOUT must be positive, got %s", c.ReadHeaderTimeout)
	}

	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("GATEWAY_SHUTDOWN_TIMEOUT must be positive, got %s", c.ShutdownTimeout)
	}

	return nil
}

// SlogLevel maps the configured level name onto a slog.Level.
func (c Config) SlogLevel() (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("GATEWAY_LOG_LEVEL must be one of debug, info, warn, error, got %q", c.LogLevel)
	}
}
