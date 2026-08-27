# 1. OpenAI-compatible API surface

## Context

The gateway sits between internal services and model providers. Every service
that would use it already talks to OpenAI, either directly over HTTP or through
one of the official SDKs.

A custom API would let us design a cleaner surface: no legacy fields, no
`choices` array with a single element, explicit tenant identifiers. It would
also mean every team has to rewrite its client code before it can migrate, and
no existing tool would work out of the box.

## Decision

Expose the OpenAI chat completions API unchanged: same path, same request and
response bodies, same error envelope, same server-sent event stream. Migration
is a base URL change and nothing else.

The gateway only adds information that clients can ignore: `X-Gateway-*`
response headers carrying token counts, cost and remaining budget.

## Consequences

- Existing SDKs, curl snippets and tools work with no modification.
- Provider-specific features that OpenAI has no field for cannot be expressed.
  Anthropic-only parameters, for example, have no home in the request.
- Non-OpenAI providers need translation in both directions, and that mapping is
  lossy by definition. Each provider implementation owns its own mapping.
- The response shape is not ours to change, so anything the gateway wants to
  report has to go in headers or in the audit log.
