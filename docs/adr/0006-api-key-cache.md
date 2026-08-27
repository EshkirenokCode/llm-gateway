# 6. Caching API key lookups

## Context

Every request has to resolve a key to a tenant. Done naively that is a database
round trip per request, which puts Postgres on the hot path of a service whose
whole point is low overhead.

## Decision

Cache the lookup result in memory for a configurable TTL, 30 seconds by
default. Cache rejections as well: an unknown or revoked key should not be able
to generate database load simply by being retried.

Do not cache transient failures. A connection error must never be remembered as
"this key is invalid".

## Consequences

- Steady traffic costs one lookup per key per TTL instead of one per request.
- Revocation is not immediate: a revoked key keeps working for up to the TTL.
  The README says so and `gwctl key revoke` repeats it. Shortening the TTL
  trades database load for faster revocation.
- The cache is per instance, so two instances can briefly disagree about a key.
- The map is bounded by dropping everything once it passes ten thousand
  entries, which in practice only happens under a key-guessing attack.
