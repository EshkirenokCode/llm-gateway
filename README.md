# llm-gateway

An OpenAI-compatible HTTP gateway that sits between your services and the model
providers. Point your existing client at it instead of `api.openai.com` and you
get per-tenant keys, token and cost accounting, budgets and failover without
touching application code.

Work in progress. See [Status](#status) for what actually works today.

## Why

Once more than one team calls an LLM provider directly you lose the ability to
answer basic questions: which team spent the money, who can be cut off when a
budget runs out, what happens when the provider is down, and how to rotate a
key that is copy-pasted into five repositories. A gateway is the one place
where those questions have answers.

## Quickstart

```
docker compose -f deploy/docker-compose.yml up -d --build
```

This starts the gateway on `:8080`, Prometheus on `:9090` and Grafana on
`:3000` (admin/admin) with a provisioned dashboard. No API keys are needed: the
default provider is a local mock.

```
curl http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}]}'
```

Add `"stream": true` to get a server-sent event stream instead.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/chat/completions` | chat completion, streaming and non-streaming |
| GET | `/healthz` | liveness: the process is running |
| GET | `/readyz` | readiness: dependencies are usable, and not draining |
| GET | `/metrics` | Prometheus metrics |

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `GATEWAY_HTTP_ADDR` | `:8080` | listen address |
| `GATEWAY_LOG_LEVEL` | `info` | debug, info, warn or error |
| `GATEWAY_READ_HEADER_TIMEOUT` | `10s` | how long a client may take to send headers |
| `GATEWAY_SHUTDOWN_TIMEOUT` | `15s` | grace period for in-flight requests on SIGTERM |

Invalid values fail at startup rather than at the first request.

## Metrics

Beyond request counts and latency, the gateway exports
`gateway_overhead_seconds`: the request duration minus the time spent waiting
for the provider. That is the only number that says what the gateway itself
costs, and it is the one worth watching.

## Status

Working:

- chat completions against a mock provider, streaming and non-streaming
- client disconnect cancels the upstream call
- config validation, graceful shutdown, liveness and readiness probes
- Prometheus metrics and a Grafana dashboard, all wired up in compose

Not built yet: tenants and API keys, cost accounting and budgets, rate
limiting, idempotency, real providers, retries and failover, circuit breaking,
audit log. Those are the next phases.

## Development

```
make test         # go test -race
make test-docker  # same, in Linux, when the host has no C compiler
make lint         # golangci-lint
make build        # ./bin/gateway
```

## License

Apache-2.0.
