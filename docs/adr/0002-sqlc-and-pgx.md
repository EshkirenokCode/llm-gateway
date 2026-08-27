# 2. sqlc and pgx instead of an ORM

## Context

The gateway needs a handful of queries: look up a key, sum tenant spend, insert
a request row, read and write idempotency records. All of them sit on the
request path and two of them run on every single request.

The alternatives considered were an ORM (GORM), a query builder (squirrel), and
hand-written SQL against `database/sql`.

## Decision

Write the SQL by hand and generate typed Go from it with sqlc, running on
pgx/v5.

## Consequences

- The SQL that runs is the SQL in the repository. There is no query generation
  layer to reason about when something is slow, and `EXPLAIN` output matches
  what is on disk.
- Query results are typed structs, so a column rename breaks the build instead
  of failing at runtime.
- pgx speaks the binary protocol and skips the `database/sql` conversions, so
  the per-request lookup stays cheap.
- Changing a query means editing SQL and running `sqlc generate`. Contributors
  need the tool installed, and the generated code is committed.
- Dynamic queries are awkward. If filtering ever has to be composed at runtime,
  that one query will be written by hand against pgx.
