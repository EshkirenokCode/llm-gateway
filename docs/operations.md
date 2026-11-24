# Operations

## Deployment boundary

This release is a single-process gateway for text chat. Build with Go 1.27.4 or newer, which includes security fixes missing from earlier patch releases. CI reads its compiler version from `go.mod`; the container builder uses the same patched release. Run `make vuln` after dependency or compiler updates. Deploy behind a TLS reverse proxy. Keep Prometheus and Grafana on a private network, replace Grafana's demo password, and set `GATEWAY_METRICS_KEY` if exposing the metrics route. If using that key, configure matching Prometheus bearer authentication; the demo scrape configuration intentionally has none.

The playground, model IDs, provider mode and health endpoints are public. Restrict `/` at the reverse proxy if these details should be private. No CORS headers are enabled. Tenant credentials belong in a secret store or deployment environment, never in committed configuration. Rotate them by replacing the environment and restarting the service.

The binary listens on all interfaces by default; use `GATEWAY_HTTP_ADDR=127.0.0.1:8080` when running it directly for local-only access. Compose maps all demo ports to host loopback.

Back up existing monitoring volumes before upgrading Prometheus or Grafana images, especially across major Grafana versions. The demo stack is verified with fresh, isolated volumes.

## Capacity and quotas

Requests are limited to 1 MiB, headers to 32 KiB, upstream JSON to 8 MiB and each upstream stream event (including all its data lines) to 1 MiB. Header reads, body reads, idle connections, upstream requests and shutdown all have deadlines. There is no application queue: a saturated tenant receives 429 immediately.

Request and concurrency limits are process-local. Multiple replicas do not share quota state. Restarting clears counters. Do not present these controls as durable budgets or exactly-once execution. The gateway does not persist request bodies, token records or audit logs. `/v1/usage` exposes only the authenticated tenant's in-memory totals and quota snapshot; it does not consume a request or concurrency slot. A saturated tenant can still inspect it. Lifetime totals survive minute-window resets but not process restarts. Token metrics come from upstream usage; a disconnected stream may not deliver final counts. Mock token counts are estimates.

Financial billing, durable budgets, distributed rate limits, idempotency, automatic failover and circuit breakers are not part of this release. Keep provider-side spend limits enabled. Clients should decide whether retrying a failed attempt is safe. A successful HTTP status on a stream only means streaming started; require the `[DONE]` sentinel for success.

## Observability

- `gateway_http_requests_total`: route, method and HTTP status.
- `gateway_http_request_duration_seconds`: full handler duration, including auth and validation.
- `gateway_http_requests_in_flight`: active API handlers, including streams.
- `gateway_upstream_duration_seconds`: provider duration and outcome.
- `gateway_overhead_seconds`: non-streaming chat handler duration minus provider time, including validation and response writing. Excludes authentication and routing; this is not a full network proxy benchmark.
- `gateway_tokens_total`: reported prompt and completion tokens, labeled with the configured request model to keep label cardinality bounded.
- `gateway_streams_total`: completion, cancellation and failure outcomes. Stream failures can occur after HTTP 200, so watch this metric alongside HTTP statuses. Deadline expiry is recorded as `timeout`, separately from `client_gone`, and a stream is counted as completed only after its sentinel is successfully flushed.

Logs use structured JSON. Correlate upstream failures with the `X-Request-ID` returned to the client. Prompts, model responses and authorization headers are not logged. `/readyz` does not check upstream reachability; alert on request failures as well as process health.

## Shutdown

SIGINT or SIGTERM marks readiness as draining, stops new connections and gives active requests the configured grace period. After the deadline, remaining connections are forcibly closed. A second signal exits immediately. Use an orchestrator termination grace period longer than `GATEWAY_SHUTDOWN_TIMEOUT`.
