# Pooled Introspection Design

**Date:** 2026-09-19
**Status:** Approved design
**Baseline:** `f19d654` (`main`)

## Purpose

Expose the two introspection capabilities that currently exist only on the
explicit direct API — `Attachment.Diagnostics` (`direct.go:440`) and
`Transaction.Plan` (`direct.go:1018`) — to callers using the pooled
`database/sql` path, without opening a second native attachment.

The consumer is the `sqls` language server. It needs `DatabaseDiagnostics.SQLDialect`
to decide whether a database is SQL Dialect 1 or 3, and it needs a
server-generated plan for an "Explain SQL" code action. Both must work on a
connection borrowed from an `*sql.DB` pool, and planning must never execute the
statement being explained.

## Evidence and Constraints

`Attachment` is a thin wrapper over the same unexported `*conn` that
`database/sql` receives: `type conn struct` (`interbase.go:719`) is referenced
by `Attachment.conn` and by every `driver.Conn` method. `interbase.go` already
imports `database/sql/driver`, and the package already imports `database/sql`
for `sql.IsolationLevel`/`sql.TxOptions` handling. No new dependency is needed.

`(*sql.Conn).Raw` hands the driver connection to a callback while holding the
`driverConn` mutex, and does not refuse to run while an `*sql.Tx` started from
the same `*sql.Conn` is open. This makes "plan inside the caller's explicit
transaction" reachable from the pool: `sql.Conn.BeginTx` then `sql.Conn.Raw`,
used sequentially, reach the same `*conn`.

`(*conn).IsValid` (`interbase.go:1346`) is what keeps a broken attachment out of
the pool; `database/sql` calls it through `putConn`/`validateConnection` when the
borrowed connection is released. `invalidateLocked` (`interbase.go:1352`) is what
makes `IsValid` report false.

### Corrections to the original proposal

Three details in the sub-project brief do not match the code and are corrected
here. They simplify the design rather than complicate it.

1. **Transaction selection needs no Go-side branch.** The proposal describes
   `Plan` choosing between the active explicit transaction and a new implicit
   one. `ib_statement_prepare_mode` (`native.c:7801`) already does exactly this:
   it uses `connection->transaction` when one is active, otherwise calls
   `ib_start_transaction(connection, ..., read_only = 1, ...)` and owns it. The
   Go side only calls `c.native.prepare(ctx, query)`.
2. **The implicit prepare transaction is committed, not rolled back.** On the
   success path `ib_statement_prepare_mode` calls `isc_commit_transaction` on
   its owned transaction before returning the statement (`native.c:7923`);
   rollback happens only on the failure path (`native.c:7948`). Because the
   transaction is read-only and nothing was executed in it, commit and rollback
   are equivalent in effect. The owned transaction's entire lifetime is inside
   the native prepare call, so `Plan` never leaves a transaction open on the
   pooled connection — a stronger property than the proposal claimed.
3. **The implicit TPB is the connector's read-only TPB, not a fixed one.**
   `ib_start_transaction` prefers `connection->read_tpb`, which the driver
   installs at attach time from `buildTPB(driver.TxOptions{ReadOnly: true},
   cfg.TransactionOptions)` (`native.go:910`, `native.go:953`). It is read-only
   and read-committed, but `TransactionOptions` wait and record-version policy
   and table reservations apply. The hardcoded read/wait/read-committed/
   `rec_version` block is only the fallback when no default TPB was installed.

One further consequence is new, not a correction: the connection-level
`ib_statement_prepare` is the `allow_arrays = 0` variant (`native.c:7962`), while
`Transaction.Plan` prepares with `allowArrays = true`. A `SELECT` whose output
includes an array column therefore fails output-type validation during pooled
`Plan` instead of returning a plan. This design accepts that: it is exactly what
`(*sql.DB).PrepareContext` already does with the same query on the same path, so
pooled `Plan` stays truthful about what the pooled path can prepare. Adding an
array-capable connection-level prepare entry point is out of scope.

## Scope

### Included

- A `Introspector` interface describing the two methods, asserted against the
  value passed to `(*sql.Conn).Raw`.
- Package-level `Diagnostics(ctx, *sql.Conn)` and `Plan(ctx, *sql.Conn, query)`
  helpers that wrap `Raw`, assert, and return a typed error otherwise.
- `(*conn).Diagnostics` and `(*conn).Plan` implementing that interface, reusing
  the existing `DatabaseDiagnostics` struct unchanged.
- Refactoring the diagnostics decoding out of `Attachment.Diagnostics` so both
  entry points share one implementation.
- A `planOverride` test seam on `nativeStatement`, matching its existing
  `execOverride`/`closeOverride`/`numInputOverride` fields.
- Unit tests, live integration tests, and README documentation.

### Excluded

- Any `DatabaseInfo`/`InfoItem` passthrough on the pooled path.
- A pooled equivalent of `Transaction.Info`.
- Array binding or array-producing plans through `Raw`.
- BLOB streaming, cursor naming, ChangeView, or any other direct-only surface
  through `Raw`.
- New cancellation guarantees. `Plan` inherits the existing best-effort DSQL
  cancellation contract and adds nothing to it.
- Executing anything. `Plan` prepares and never executes, in any mode.

Only what `sqls` consumes is added. Further direct-API surface reaches the pool
only when a concrete consumer exists.

## Architecture

### Public surface

```go
// Introspector is implemented by the driver connection passed to
// (*sql.Conn).Raw. The value must not be retained beyond the callback.
type Introspector interface {
    Diagnostics(ctx context.Context) (DatabaseDiagnostics, error)
    Plan(ctx context.Context, query string) (string, error)
}

// ErrNotInterBaseConn reports a connection that does not belong to this driver.
var ErrNotInterBaseConn = errors.New("interbase: connection is not an InterBase connection")

func Diagnostics(ctx context.Context, conn *sql.Conn) (DatabaseDiagnostics, error)
func Plan(ctx context.Context, conn *sql.Conn, query string) (string, error)
```

Each helper calls `conn.Raw`, asserts the callback value to `Introspector`,
invokes the method, copies the result into a variable owned by the helper, and
returns the driver error unchanged from the callback. A nil `*sql.Conn` returns
`ErrNotInterBaseConn` rather than panicking. The interface is exported so that
callers who already hold a `Raw` callback for other reasons are not forced
through the helpers; the helpers remain the documented API.

### `(*conn).Diagnostics`

The body of `Attachment.Diagnostics` moves to `(*conn).Diagnostics` verbatim:
`contextError`, `lockDirect()`/`defer c.mu.Unlock()`, the
closed/nil-native/broken check returning `driver.ErrBadConn`,
`enterNativeContext(ctx)`, the six `databaseInfo` reads decoded through
`parseInfoItem`, `clientVersion()`, and `sanitizeError` on every failure.
`Attachment.Diagnostics` keeps its nil-receiver check returning
`errDirectAttachmentClosed` and then delegates, so the two paths cannot drift.

### `(*conn).Plan`

`Plan` mirrors `Transaction.Plan` with the pooled path's validator and prepare
entry point:

1. `contextError(ctx)`, then `validateDatabaseSQLQuery(query)` — the pooled
   validator, so `SET SUBSCRIPTION` is redirected to the direct API exactly as
   `PrepareContext` does, rather than the direct-only `validateQueryText`.
2. `c.lockDirect()` and `defer c.mu.Unlock()`.
3. Return `ErrDistributedParticipantManaged` when `c.distributed != nil`. Today
   only explicit attachments can be distributed participants, so this is
   unreachable from the pool; it is kept because the method lives on the shared
   `*conn` and `BeginTx` already sets this precedent for implicit transaction
   selection on a coordinator-managed connection.
4. Return `driver.ErrBadConn` when `c.closed`, `c.native == nil`, or
   `c.native.broken()`.
5. `enterNativeContext(ctx)`, released on every path.
6. `c.native.prepare(ctx, query)`. Native code selects the connection's active
   explicit transaction when `BeginTx` started one and has not completed it, and
   otherwise starts, uses, and completes its own read-only transaction inside the
   call. Go makes no transaction decision.
7. On prepare failure, classify exactly as `PrepareContext` does:
   `nativeCancellationEvidenceOf`, `splitNativeExecutionError`, `sanitizeError`
   for primary and cleanup parts, `classifyNativeOutcome("prepare plan", false, …)`
   with the non-mutating flag, `joinNativeRequestDiagnostic`, then
   `invalidateLocked` when `c.native.broken()`. Native prepare already closed its
   own statement and rolled back its own transaction before returning the error.
8. On success, re-check `contextError(ctx)` and return `errors.Join(err,
   statement.close())` if it fires.
9. Otherwise read `statement.plan()`, then `statement.close()` unconditionally,
   join the two errors, `sanitizeError` them, and `invalidateLocked` if the
   connection broke.

The prepared statement is never registered in `c.statements` and never returned
to the caller. It exists only between steps 6 and 9.

### Why this is prepare-only

`isc_dsql_prepare` is the only DSQL call `Plan` makes. No `isc_dsql_execute`,
`execute2`, or `fetch` is reachable from this path, for any statement type. A
`DELETE` or `UPDATE` passed to `Plan` is compiled and described, and its
statement handle is closed. The existing documented caveat carries over: a valid
DML statement may return an empty plan string, and an empty plan never implies
that the DML ran.

## Result and Error Semantics

| Condition | Result |
| --- | --- |
| Success | `DatabaseDiagnostics` / plan string, copied out of the callback. |
| `conn` is nil or belongs to another driver | `ErrNotInterBaseConn`. |
| `*sql.Conn` already closed | `sql.ErrConnDone` from `Raw`, unchanged. |
| Driver connection closed, nil-native, or broken on entry | `driver.ErrBadConn`. |
| Context canceled before or during the operation | The existing DSQL cancellation semantics, unchanged. |
| Native failure | The sanitized, classified operation error. |
| Native failure that broke the attachment | The same error, and `IsValid` now reports false. |

The helpers never rewrite an error. In particular they never manufacture
`driver.ErrBadConn`, and never downgrade a classified cancellation error to it.
Returning `driver.ErrBadConn` out of the `Raw` callback causes `database/sql` to
close that `*sql.Conn`; that is correct when the attachment really is unusable,
and `Raw` is never retried, so there is no replay hazard.

`SQLDialect` is decoded from `InfoDatabaseSQLDialect` and reports the database's
dialect as the server answers for this attachment. It is the server's value, not
an echo of `Config.Dialect`, and consumers should treat it as the authoritative
answer for dialect auto-detection.

Cancellation adds nothing new: prepare is already a cancelable DSQL operation
with a cancellation watcher, so `Plan` inherits best-effort interruption, the
rule that the executing native result stays authoritative, and the rule that
cancellation alone never becomes `driver.ErrBadConn`. `Diagnostics` uses
`isc_database_info`, which is not a DSQL operation and is not cancelable; it
checks `contextError` between reads, which is the behavior `Attachment.Diagnostics`
already has.

## Concurrency and Lifecycle Rules

- Both methods take `c.mu` through `lockDirect()` and hold it for the whole
  operation, so they serialize against every other connection operation exactly
  as the direct API does. `(*sql.Conn).Raw` additionally holds the `driverConn`
  mutex, so no second `database/sql` operation on that connection can overlap.
- `enterNativeContext` admission is acquired and released on every path,
  including the early-error paths.
- The `Introspector` value is valid only inside the `Raw` callback. The helpers
  do not store it, and the documentation states that callers must not either.
- A native failure that sets `broken()` triggers `invalidateLocked`, which closes
  rows and statements, clears `c.native`, and makes `IsValid` false; the pool
  then discards the connection when the `*sql.Conn` is released or closed.
- `Plan` leaves no transaction state behind. It neither begins nor completes a
  caller-owned `database/sql` transaction, and the implicit transaction it may
  cause is created and completed inside the native prepare call.
- No goroutines are introduced beyond the existing per-operation cancellation
  watcher, which is joined inside `c.native.prepare` before it returns.

## Testing Strategy

### Unit tests, no server (root package)

These build a `*conn` with a fake `nativeConnection` and expose it through
`openDatabaseSQLTestDB` (`connect_timeout_test.go:31`), the existing seam that
wraps a `driver.Conn` in a real `*sql.DB`. They then call the public helpers
with a `*sql.Conn` from that pool.

- `Diagnostics` decodes every field, using the `databaseInfoOverride` and
  `clientVersionOverride` response encoding already used by
  `TestAttachmentDiagnosticsDecodesNativeInfoAndClientVersion`
  (`direct_lifecycle_test.go:869`). Includes a `SQLDialect` payload of `1` and of
  `3`, asserting the decoded value.
- `Attachment.Diagnostics` and the pooled `Diagnostics` return identical structs
  for the same fake native responses, proving the shared implementation.
- `Plan` returns the string produced by a `planOverride`, and the statement's
  `closeOverride` is observed to have run exactly once.
- `Plan` closes the statement when `planOverride` fails, and the returned error
  joins both failures.
- `Plan` on a query rejected by `validateDatabaseSQLQuery` (NUL byte, oversized,
  `SET SUBSCRIPTION`) never reaches `prepareOverride`.
- Closed connection: after `(*conn).Close`, both helpers return
  `driver.ErrBadConn` through `errors.Is`, and `prepareOverride`/
  `databaseInfoOverride` are never called.
- Wrong driver: a `*sql.Conn` from a `*sql.DB` backed by a non-InterBase
  `driver.Conn` returns `ErrNotInterBaseConn` from both helpers. A nil `*sql.Conn`
  returns the same error without panicking.
- Broken during introspection: `prepareOverride` (and separately
  `databaseInfoOverride`) returns a native error while `brokenOverride` reports
  true. The test asserts the error propagates, that `(*conn).IsValid()` is false
  afterwards, and that closing the `*sql.Conn` and reopening from the same
  `*sql.DB` does not hand back the same `*conn` pointer.
- Context cancellation during prepare: a `prepareContextOverride` blocks until
  the test cancels, following the pattern of
  `TestDatabaseSQLPreparedExecContextCancelsActiveNativeCall`. The returned error
  matches the originating context through `errors.Is`, and the connection mutex
  is shown to be released afterwards by a successful subsequent `Diagnostics`
  call on a fresh connection.

### Live tests, `integration/` package

All new live tests carry `//go:build integration`, live in
`integration/introspection_test.go`, and use the existing fixture helpers
(`newDatabaseWithDialect`, `createFixture`, `openDatabase`, `readContext`), so
they are gated by the same build tag and `testfixture.FromEnv()` configuration as
every other live test and are covered by `make test-integration-docker`.

- `TestIntrospectionDiagnosticsReportsDialect`: table-driven over dialects 1 and
  3 using `newDatabaseWithDialect(t, dialect)`. Asserts
  `diagnostics.SQLDialect == int64(dialect)` and that `ClientVersion`,
  `ServerVersion`, and `PageSize` are populated, mirroring the assertions in
  `TestDirectLifecycleDiagnosticsAndDMLPlanDoNotExecute`
  (`integration/lifecycle_test.go:234`).
- `TestIntrospectionPlanReturnsSelectPlan`: `Plan` for
  `SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?` returns a non-empty plan.
- `TestIntrospectionPlanForDMLDoesNotExecute`: the safety proof. Uses
  `createFixture` plus two independent pools on the same fixture. Reads the
  sentinel row from the first pool, calls `Plan` with
  `UPDATE GO_COUNTRY SET COUNTRY = 'changed' WHERE ID = 1` and with
  `DELETE FROM GO_COUNTRY WHERE ID = 1`, then asserts on row state, not on
  absence of error: the same `*sql.Conn` still reads `COUNTRY = 'USA'` for
  `ID = 1`, the row count for `ID = 1` is still 1, and the second pool — a
  separate native attachment — reads the same values. The plan strings are only
  logged, because an empty DML plan is permitted.
- `TestIntrospectionPlanUsesActiveTransaction`: on one `*sql.Conn`, begin a
  writable explicit transaction, `UPDATE` a row inside it, then call `Plan` on a
  `SELECT` of that row through `Raw` on the same `*sql.Conn`, and also plan
  `SELECT ... FOR UPDATE`, which only prepares successfully inside a writable
  transaction. Both return non-empty plans; the same `Plan` calls run before
  `BeginTx` are used as the contrast case for `FOR UPDATE`. Rolling back
  afterwards leaves the table unchanged, proving `Plan` did not complete the
  caller's transaction.
- `TestIntrospectionPlanLeavesNoOpenTransaction`: after `Plan` without an
  explicit transaction, an immediate `ExecContext` insert on the same `*sql.Conn`
  commits normally and is visible from the second pool, showing the implicit
  prepare transaction did not linger.
- `TestIntrospectionOnClosedConn`: `sql.Conn.Close()` then both helpers return
  `sql.ErrConnDone`.

`make test` must stay green without a server: every new test that needs one is
behind the `integration` tag.

## Documentation and Compatibility

This is additive. No existing signature, error, or behavior changes;
`DatabaseDiagnostics`, `Attachment.Diagnostics`, and `Transaction.Plan` keep
their current contracts, and `Attachment.Diagnostics` keeps returning the same
values through the extracted implementation.

README changes:

- "Go Usage" gains a short pooled-introspection example showing
  `db.Conn(ctx)` followed by `interbase.Diagnostics` and `interbase.Plan`, and
  states that they avoid a second native attachment.
- "Supported Boundary" adds pooled `Diagnostics` and `Plan`, and states that
  pooled `Plan` prepares only, uses the connection's explicit transaction when
  one is active, and otherwise uses a read-only transaction that begins and ends
  inside the native prepare call.
- "Known Limits" records that a pooled `Plan` of a `SELECT` with an array output
  column fails output-type validation, matching pooled `PrepareContext`, and that
  a valid DML statement may return an empty plan while an empty plan never means
  the statement ran.

The supported platform scope is unchanged: Linux/amd64 against the tested
InterBase 15.1 client/server stack. No latency, thread-safety, or plan-format
claim is added.
