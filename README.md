# llm-gateway

An OpenAI-compatible HTTP gateway that sits between your services and the model
providers. Point an existing client at it instead of `api.openai.com` and you
get per-tenant API keys, token and cost accounting, budgets and rate limits
without touching application code.

Work in progress. See [Status](#status) for what actually works today.

## Why

Once more than one team calls a model provider directly you lose the ability to
answer basic questions: which team spent the money, who gets cut off when a
budget runs out, what happens when the provider is down, and how to rotate a
key that has been copy-pasted into five repositories. A gateway is the one
place where those questions have answers.

```
  service A ──┐                                  ┌──> OpenAI
  service B ──┼──> gateway ──> auth, budget, ────┤
  service C ──┘        │       accounting        └──> Anthropic
                       v
                  Postgres, Prometheus
```

## Quickstart

```
docker compose -f deploy/docker-compose.yml up -d --build
```

That starts the gateway on `:8080`, Postgres on `:5432`, Prometheus on `:9090`
and Grafana on `:3000` (admin/admin) with a provisioned dashboard. Migrations
run at startup. No provider API keys are needed: the default provider is a
local mock.

Create a tenant and a key:

```
export DATABASE_URL=postgres://gateway:gateway@localhost:5432/gateway?sslmode=disable
go run ./cmd/gwctl tenant create -name "Acme" -prefix acme -budget 25
go run ./cmd/gwctl key create -tenant <TENANT_ID> -name prod -rpm 120
```

The plaintext key is printed once and never stored; the database keeps only its
sha256. Then:

```
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk_acme_..." \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}]}'
```

Add `"stream": true` for a server-sent event stream, or an `Idempotency-Key`
header to make a retry safe.

Running without `DATABASE_URL` is also supported: the gateway then works as an
open proxy with metrics but no tenants, accounting or budgets.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/chat/completions` | chat completion, streaming and non-streaming |
| GET | `/healthz` | liveness: the process is running |
| GET | `/readyz` | readiness: dependencies are usable, and not draining |
| GET | `/metrics` | Prometheus metrics |

Responses carry `X-Gateway-Provider`, `X-Gateway-Tokens-Total`,
`X-Gateway-Cost-Usd` and, for tenants with a budget,
`X-Gateway-Budget-Remaining-Usd`.

Status codes worth knowing: `401` unknown or revoked key, `402` monthly budget
exhausted, `409` an `Idempotency-Key` reused with a different body, `429` rate
limit, `502` the provider failed.

## Tenants, keys and money

- A tenant has a prefix (`acme`), an optional monthly budget and a soft limit
  ratio, 0.8 by default.
- Keys look like `sk_acme_<random>`. Only the sha256 is stored, along with the
  last four characters so a key can be identified in a list.
- Spend is recorded per request in micro dollars, computed from the token
  counts the provider reports and the price list in `deploy/pricing.json`.
- Crossing the soft limit adds `X-Gateway-Budget-Warning` to the response.
  Crossing the budget itself returns `402` and the request never reaches the
  provider.
- Rate limits are per key, enforced with a token bucket in memory.

Revoking a key takes effect within `GATEWAY_KEY_CACHE_TTL`, 30 seconds by
default, because key lookups are cached. This is a deliberate trade, written up
in [adr/0006](docs/adr/0006-api-key-cache.md).

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `GATEWAY_HTTP_ADDR` | `:8080` | listen address |
| `GATEWAY_LOG_LEVEL` | `info` | debug, info, warn or error |
| `GATEWAY_READ_HEADER_TIMEOUT` | `10s` | how long a client may take to send headers |
| `GATEWAY_SHUTDOWN_TIMEOUT` | `15s` | grace period for in-flight requests on SIGTERM |
| `DATABASE_URL` | empty | Postgres connection string; empty disables persistence |
| `GATEWAY_DB_MAX_CONNS` | `10` | connection pool size |
| `GATEWAY_MIGRATE_ON_START` | `true` | apply pending migrations at startup |
| `GATEWAY_PRICING_FILE` | empty | JSON price list; falls back to built-in prices |
| `GATEWAY_KEY_CACHE_TTL` | `30s` | how long a key verdict is reused |
| `GATEWAY_BUDGET_CACHE_TTL` | `15s` | how long a tenant spend total is reused |
| `GATEWAY_IDEMPOTENCY_TTL` | `24h` | how long a stored response can be replayed |
| `GATEWAY_RATE_LIMIT_BURST_RATIO` | `0.25` | share of the per-minute allowance spendable at once |

Invalid values fail at startup rather than at the first request.

## Metrics

Beyond request counts and latency, the gateway exports
`gateway_overhead_seconds`: request duration minus the time spent waiting for
the provider. That is the only number that says what the gateway itself costs.
Also exported: `gateway_tokens_total`, `gateway_cost_usd_total`,
`gateway_blocked_total` by reason, and `gateway_streams_total` by outcome.

## Status

Working:

- chat completions against a mock provider, streaming and non-streaming
- client disconnect cancels the upstream call
- tenants, API keys stored as hashes, per-key rate limits
- token and cost accounting per request, monthly budgets with soft and hard
  limits
- `Idempotency-Key` replay, including a conflict when the body differs
- Postgres schema with goose migrations applied at startup
- Prometheus metrics and a provisioned Grafana dashboard

Not built yet: real providers (only the mock exists), retries and failover,
circuit breaking, the audit log is written by the storage layer but nothing
calls it on the request path yet, and prompt redaction is not implemented.
Those are the next phase.

Known limitations:

- rate limits are per instance, see [adr/0005](docs/adr/0005-in-memory-rate-limiting.md)
- key revocation lags by the cache TTL, see [adr/0006](docs/adr/0006-api-key-cache.md)
- a stream aborted before its final chunk is recorded with zero tokens, see
  [adr/0003](docs/adr/0003-token-counting.md)
- there is no admin HTTP API; tenants and keys are managed with `gwctl`

## Development

```
make test         # go test -race, including integration tests against a
                  # throwaway Postgres started with testcontainers
make test-short   # unit tests only, no docker needed
make test-docker  # full suite in Linux, for hosts without a C compiler
make lint         # golangci-lint
make build        # ./bin/gateway and ./bin/gwctl
```

Regenerating the database layer after changing a query needs
[sqlc](https://sqlc.dev):

```
sqlc generate
```

## License

Apache-2.0.
