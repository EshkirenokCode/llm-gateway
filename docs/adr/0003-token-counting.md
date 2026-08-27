# 3. Where token counts come from

## Context

Cost is token count times price. The gateway can get that count two ways: count
the tokens itself with a tokenizer, or use the numbers the provider reports.

Counting locally means shipping a tokenizer per model family, keeping those in
sync with provider changes, and spending CPU on the request path. It does give
an answer before the request is made, which is tempting for budget checks.

## Decision

Take the counts from the provider response. For streamed responses, take them
from the final chunk, where usage is reported.

Do not attempt a local estimate for real providers. The mock provider estimates
because it has no upstream to ask.

## Consequences

- The recorded numbers match the provider invoice, which is the number that
  matters when someone asks where the money went.
- Nothing is known before the call, so budgets are enforced on spend so far,
  not on the projected cost of the request in hand. One very large request can
  overshoot the cap; the next request is refused.
- A stream aborted before the final chunk reports no usage. Those requests are
  recorded with zero tokens and zero cost, which undercharges. Guessing would
  be worse, and the loss is bounded by how often clients hang up mid-stream.
- If a provider stops reporting usage, accounting for it silently drops to
  zero. The request is still recorded, so the gap is visible in the data.
