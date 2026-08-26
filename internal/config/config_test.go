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
		HTTPAddr:          ":8080",
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
