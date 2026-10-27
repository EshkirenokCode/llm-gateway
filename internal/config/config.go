// Package config loads and validates the gateway configuration from the
// environment. Configuration is read once at startup; an invalid value is a
// fatal error, so the process never runs in a half-configured state.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/EshkirenokCode/llm-gateway/internal/tenant"
	"github.com/caarlos0/env/v11"
)

// Config holds every runtime setting of the gateway.
type Config struct {
	Provider       string        `env:"GATEWAY_PROVIDER" envDefault:"mock"`
	UpstreamURL    string        `env:"GATEWAY_UPSTREAM_URL"`
	UpstreamKey    string        `env:"GATEWAY_UPSTREAM_KEY"`
	RequestTimeout time.Duration `env:"GATEWAY_REQUEST_TIMEOUT" envDefault:"60s"`
	Models         []string      `env:"GATEWAY_MODELS" envDefault:"demo-model" envSeparator:","`
	TenantsJSON    string        `env:"GATEWAY_TENANTS"`
	MetricsKey     string        `env:"GATEWAY_METRICS_KEY"`

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

	if c.Provider != "mock" && c.Provider != "compatible" {
		return fmt.Errorf("GATEWAY_PROVIDER must be mock or compatible")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("GATEWAY_REQUEST_TIMEOUT must be positive")
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("GATEWAY_MODELS must not be empty")
	}
	seenModels := map[string]bool{}
	for _, m := range c.Models {
		if strings.TrimSpace(m) != m || m == "" || seenModels[m] {
			return fmt.Errorf("GATEWAY_MODELS must contain distinct nonempty model IDs")
		}
		seenModels[m] = true
	}
	tenants, err := c.Tenants()
	if err != nil {
		return err
	}
	if c.Provider == "compatible" {
		u, err := url.Parse(c.UpstreamURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("GATEWAY_UPSTREAM_URL must be an HTTP(S) API base URL without credentials or query")
		}
		if len(tenants) == 0 {
			return fmt.Errorf("GATEWAY_TENANTS is required for a compatible upstream")
		}
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

func (c Config) Tenants() ([]tenant.Policy, error) {
	if strings.TrimSpace(c.TenantsJSON) == "" {
		return nil, nil
	}
	var tenants []tenant.Policy
	dec := json.NewDecoder(strings.NewReader(c.TenantsJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tenants); err != nil {
		return nil, fmt.Errorf("GATEWAY_TENANTS must be a JSON array of name, key, rpm and concurrency")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("GATEWAY_TENANTS must contain one JSON array")
	}
	names, keys := map[string]bool{}, map[string]bool{}
	for _, t := range tenants {
		if strings.TrimSpace(t.Name) == "" || len(t.Key) < 24 || strings.TrimSpace(t.Key) != t.Key || t.RPM <= 0 || t.Concurrency <= 0 || names[t.Name] || keys[t.Key] {
			return nil, fmt.Errorf("GATEWAY_TENANTS requires unique names, unique keys of at least 24 characters, and positive limits")
		}
		names[t.Name] = true
		keys[t.Key] = true
	}
	return tenants, nil
}
