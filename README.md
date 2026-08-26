# llm-gateway

A self-hosted, OpenAI-compatible gateway that sits in front of multiple LLM
providers and adds per-tenant API keys, token/cost accounting, budgets,
provider failover and an audit trail.

Status: early development. Nothing below is implemented yet beyond the
project skeleton.

## Build

```
make build   # compile ./bin/gateway
make test    # go test -race
make lint    # golangci-lint
make docker  # container image
```
