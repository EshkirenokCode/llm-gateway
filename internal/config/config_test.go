package config_test

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EshkirenokCode/llm-gateway/internal/config"
)

// valid returns a configuration that passes validation, so each test case can
// break exactly one field and assert on that.
func valid() config.Config {
	return config.Config{
		HTTPAddr: ":8080",
		Provider: "mock", RequestTimeout: 60 * time.Second, Models: []string{"demo-model"},
		LogLevel:          "info",
		ReadHeaderTimeout: 10 * time.Second,
		ShutdownTimeout:   15 * time.Second,
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{
			name:   "valid config",
			mutate: func(*config.Config) {},
		},
		{
			name:    "empty address",
			mutate:  func(c *config.Config) { c.HTTPAddr = "  " },
			wantErr: "GATEWAY_HTTP_ADDR",
		},
		{
			name:    "unknown log level",
			mutate:  func(c *config.Config) { c.LogLevel = "verbose" },
			wantErr: "GATEWAY_LOG_LEVEL",
		},
		{
			name:    "zero read header timeout",
			mutate:  func(c *config.Config) { c.ReadHeaderTimeout = 0 },
			wantErr: "GATEWAY_READ_HEADER_TIMEOUT",
		},
		{
			name:    "negative shutdown timeout",
			mutate:  func(c *config.Config) { c.ShutdownTimeout = -time.Second },
			wantErr: "GATEWAY_SHUTDOWN_TIMEOUT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := valid()
			tt.mutate(&cfg)

			err := cfg.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestConfigSlogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want slog.Level
	}{
		{in: "debug", want: slog.LevelDebug},
		{in: "info", want: slog.LevelInfo},
		{in: "WARN", want: slog.LevelWarn},
		{in: " error ", want: slog.LevelError},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			cfg := valid()
			cfg.LogLevel = tt.in

			got, err := cfg.SlogLevel()
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestLoadUsesDefaults(t *testing.T) {
	// No t.Parallel: unsetting environment variables mutates process-wide state.
	// t.Setenv cannot unset, so restore the previous values by hand.
	for _, key := range []string{
		"GATEWAY_PROVIDER", "GATEWAY_UPSTREAM_URL", "GATEWAY_UPSTREAM_KEY", "GATEWAY_REQUEST_TIMEOUT", "GATEWAY_MODELS", "GATEWAY_TENANTS", "GATEWAY_METRICS_KEY",
		"GATEWAY_HTTP_ADDR",
		"GATEWAY_LOG_LEVEL",
		"GATEWAY_READ_HEADER_TIMEOUT",
		"GATEWAY_SHUTDOWN_TIMEOUT",
	} {
		if old, ok := os.LookupEnv(key); ok {
			t.Cleanup(func() { _ = os.Setenv(key, old) })
		}
		require.NoError(t, os.Unsetenv(key))
	}

	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, valid(), cfg)
}

func TestLoadRejectsBadValue(t *testing.T) {
	t.Setenv("GATEWAY_LOG_LEVEL", "verbose")

	_, err := config.Load()
	require.ErrorContains(t, err, "GATEWAY_LOG_LEVEL")
}

func TestCompatibleConfiguration(t *testing.T) {
	cfg := valid()
	cfg.Provider = "compatible"
	cfg.UpstreamURL = "http://localhost:11434/v1"
	cfg.TenantsJSON = `[{"name":"app","key":"example-key-with-at-least-24-characters","rpm":60,"concurrency":4}]`
	require.NoError(t, cfg.Validate())
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"missing tenant", func(c *config.Config) { c.TenantsJSON = "" }},
		{"short key", func(c *config.Config) { c.TenantsJSON = `[{"name":"app","key":"short","rpm":1,"concurrency":1}]` }},
		{"credential in URL", func(c *config.Config) { c.UpstreamURL = "https://user:password@example.com/v1" }},
		{"invalid scheme", func(c *config.Config) { c.UpstreamURL = "file:///etc/passwd" }},
		{"query in URL", func(c *config.Config) { c.UpstreamURL = "https://example.com/v1?key=secret" }},
		{"unknown tenant field", func(c *config.Config) { c.TenantsJSON = `[{"password":"do-not-expose"}]` }},
		{"extra JSON", func(c *config.Config) { c.TenantsJSON += "{}" }},
		{"duplicate model", func(c *config.Config) { c.Models = []string{"m", "m"} }},
		{"empty model", func(c *config.Config) { c.Models = []string{""} }},
		{"invalid provider", func(c *config.Config) { c.Provider = "unknown" }},
		{"zero timeout", func(c *config.Config) { c.RequestTimeout = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := cfg
			tc.mutate(&bad)
			err := bad.Validate()
			require.Error(t, err)
			require.NotContains(t, err.Error(), "password")
			require.NotContains(t, err.Error(), "do-not-expose")
		})
	}
}

func TestCredentialsRejectUnusableHeaderValues(t *testing.T) {
	for _, key := range []string{"a key containing a space", "a-key-containing-a-newline\n", "a-key-containing-кириллица"} {
		cfg := valid()
		cfg.UpstreamKey = key
		require.Error(t, cfg.Validate())
		cfg = valid()
		cfg.MetricsKey = key
		require.Error(t, cfg.Validate())
	}
	cfg := valid()
	cfg.Provider = "compatible"
	cfg.UpstreamURL = "http://:8080/v1"
	cfg.TenantsJSON = `[{"name":"app","key":"example-key-at-least-24-characters","rpm":1,"concurrency":1}]`
	require.Error(t, cfg.Validate())
}
