// Command gateway is a self-hosted, OpenAI-compatible LLM gateway that adds
// per-tenant authentication, cost accounting, budgets and provider failover
// in front of multiple model providers.
package main

import (
	"log/slog"
	"os"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	logger.Info("gateway starting", "version", version)
}
