# 5. In-memory rate limiting

## Context

Each API key has a requests-per-minute allowance. Enforcing it accurately needs
shared counters as soon as the gateway runs as more than one instance.

The usual answer is Redis: one counter per key, atomic increments, correct
across instances. That is another service to run, another dependency on the
request path, and another decision about what to do when it is unreachable.

## Decision

Keep a token bucket per key in the memory of each instance. No shared state, no
extra service.

## Consequences

- With N instances behind a load balancer the effective limit is roughly N
  times the configured one. The limit exists to stop a runaway client, not to
  sell a quota, and that overshoot is acceptable for the purpose.
- A restart resets every bucket, so a client can burst again immediately.
- There is no dependency to fail: the limiter cannot take the gateway down, and
  there is no degraded path to get wrong.
- Buckets are swept every ten minutes; otherwise the map grows with every key
  that ever made a request.
- If a hard multi-instance quota is ever needed, this becomes a Redis-backed
  limiter behind the same `Allow` method, and nothing above it changes.
