# 4. Money as integer micro dollars

## Context

Per-request costs are tiny: a small completion can cost less than a thousandth
of a cent. They are also summed over millions of rows to answer how much a
tenant spent this month.

Floating point accumulates error over long sums, and the error depends on the
order of addition, so two runs of the same aggregate can disagree. Postgres
`numeric` is exact but slower to sum and clumsy to carry through Go.

## Decision

Store and compute money as `int64` micro dollars, where one unit is 0.000001
USD. `billing.Micros` is the type; the column type is `bigint`.

Converting tokens to money rounds up.

## Consequences

- Sums are exact and identical on every run.
- The smallest representable amount is one micro dollar. Prices below that per
  token round up, so the gateway can charge marginally more than the provider
  on very small requests. Rounding down would hand out free requests to anyone
  making enough of them.
- An `int64` of micro dollars overflows somewhere past nine trillion dollars,
  which is not a limit worth planning around.
- Every display path divides by a million. That conversion lives in one place,
  `Micros.Dollars`.
