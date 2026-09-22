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
borrowed connection is released. `IsValid` reports false if the connection is
closed, `c.native` is nil, or the native attachment reports `broken()`.
`invalidateLocked` (`interbase.go:1352`) closes the connection and clears
`c.native`, but is not required for a broken attachment to fail validation.

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
   its owned transaction before returning the statement (`native.c:7925`);
   rollback happens only on the failure path (`native.c:7950`). Because the
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
  `execOverride`/`closeOverride`/`numInputOverride` fields, and a
  connection-minting variant of `databaseSQLTestConnector` so pool eviction is
  observable.
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
`ErrNotInterBaseConn` rather than panicking.

Exporting `Introspector` is a documentation choice, not a necessity: Go
interfaces are structural, so a third party can already declare and assert an
identical interface inside their own `Raw` callback whether or not this package
names it. Exporting states which methods are the supported contract and pins its
shape, at the cost of making the two method signatures part of the public API.
The helpers remain the documented entry points.

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
3. Return `ErrDistributedParticipantManaged` when `c.distributed != nil`.
   **This branch is unreachable from the pool by design and is expected to stay
   that way:** `c.distributed` is only ever assigned through `BeginDistributed`,
   which accepts explicit `Attachment` participants (`distributed.go:216`). The
   guard exists because the method lives on the shared `*conn` and `BeginTx`
   already refuses implicit transaction selection on a coordinator-managed
   connection. A future reader should not treat it as a live path, and a test
   for it belongs to the direct API, not to this sub-project.
4. Return `driver.ErrBadConn` when `c.closed`, `c.native == nil`, or
   `c.native.broken()`.
5. `enterNativeContext(ctx)`, released on every path.
6. `c.native.prepare(ctx, query)`. Native code selects the connection's active
   explicit transaction when `BeginTx` started one and has not completed it, and
   otherwise starts, uses, and completes its own read-only transaction inside the
   call. Go makes no transaction decision.
7. On prepare failure, classify exactly as `PrepareContext` does
   (`interbase.go:1100`): `nativeCancellationEvidenceOf`,
   `splitNativeExecutionError`, the `c.sanitizeError` *method*
   (`interbase.go:733`, which applies `c.redactionSecrets`) for the primary and
   cleanup parts, then

   ```go
   operationErr = classifyNativeOutcome("prepare plan", false,
       contextCancellation(ctx), operationErr, cleanupErr, evidence)
   ```

   — note the third argument is `contextCancellation(ctx)` (`cancellation.go:573`),
   not `contextError(ctx)` — followed by `joinNativeRequestDiagnostic` and
   `invalidateLocked` when `c.native.broken()`. Native prepare already closed its
   own statement and rolled back its own transaction before returning the error.
8. On success, re-check `contextError(ctx)` and return `errors.Join(err,
   statement.close())` if it fires.
9. Otherwise read `statement.plan()`, then `statement.close()` unconditionally,
   join the two errors, pass them through `c.sanitizeError`, and
   `invalidateLocked` if the connection broke.

The prepared statement is never registered in `c.statements` and never returned
to the caller. It exists only between steps 6 and 9.

### Why this is prepare-only

The load-bearing claim is negative: **no `isc_dsql_execute`, `isc_dsql_execute2`,
or `isc_dsql_fetch` is reachable from this path, for any statement type.** The
path is not a single native call. `ib_statement_prepare_mode` issues
`isc_dsql_allocate_statement`, `isc_dsql_prepare`, `isc_dsql_describe_bind`
(`native.c:1620`), and `isc_dsql_sql_info` for `isc_info_sql_stmt_type`
(`native.c:1698`), plus `isc_dsql_describe` (`native.c:1665`) when the statement
type is `select`, `select_for_upd`, or `exec_procedure`. `statement.plan()` then
issues a second `isc_dsql_sql_info` for `isc_info_sql_get_plan`
(`native.c:8288`), and `statement.close()` issues `isc_dsql_free_statement`.
Every one of these compiles, describes, or releases; none of them runs the
statement.

A `DELETE` or `UPDATE` passed to `Plan` is therefore compiled and described, and
its statement handle is closed. The existing documented caveat carries over: a
valid DML statement may return an empty plan string, and an empty plan never
implies that the DML ran.

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
- A native failure that sets `broken()` makes `IsValid` false, so the pool
  discards the connection when it is returned via `(*sql.Conn).Close`.
  `Plan` additionally calls `invalidateLocked` on its broken-attachment error
  paths, closing rows and statements and clearing `c.native`. `Diagnostics`
  does not call `invalidateLocked`; eviction relies on `IsValid` observing the
  broken native attachment. Neither path needs to replace the operation error
  with `driver.ErrBadConn` to achieve this eviction.
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
  true. The test asserts the error propagates and that `(*conn).IsValid()` is
  false afterwards.

  Proving the pool actually discards it needs a new seam. The existing
  `databaseSQLTestConnector.Connect` returns one fixed `driver.Conn` on every
  call (`connect_timeout_test.go:17`), so the pool drops the dead `*conn` and
  immediately reconnects to the same dead `*conn`; a pointer-identity assertion
  against that connector is meaningless. Add a sibling connector whose `Connect`
  calls a `func() driver.Conn` factory and records each minted connection. With
  it, the test closes the `*sql.Conn`, acquires a new one, runs a successful
  `Diagnostics`, and asserts the factory was called a second time and that the
  first `*conn` never served it. That assertion is what proves `IsValid` removed
  the broken connection from the pool rather than merely reporting false.
- Context cancellation during prepare: a `prepareContextOverride` blocks until
  the test cancels, following the pattern of
  `TestDatabaseSQLPreparedExecContextCancelsActiveNativeCall`
  (`direct_lifecycle_test.go:864`). The returned error matches the originating
  context through `errors.Is`. A subsequent `Diagnostics` call that completes
  then shows `c.mu` was released; with the fixed-connection connector that call
  reaches the same `*conn`, which is the stronger demonstration and is what the
  test should assert.

### Live tests, `integration/` package

All new live tests carry `//go:build integration`, live in
`integration/introspection_test.go`, and use the existing fixture helpers
(`newDatabaseWithDialect`, `createFixture`, `openDatabase`, `readContext`), so
they are gated by the same build tag and `testfixture.FromEnv()` configuration as
every other live test and are covered by `make test-integration-docker`.

`openDatabase` sets `SetMaxOpenConns(1)`, so any test that needs two concurrent
attachments must call `createFixture` once and `openDatabase` twice against the
same fixture path rather than taking two `*sql.Conn` from one pool.

- `TestIntrospectionDiagnosticsReportsDialect`: table-driven over dialects 1 and
  3 using `newDatabaseWithDialect(t, dialect)`. Asserts
  `diagnostics.SQLDialect == int64(dialect)` and that `ClientVersion`,
  `ServerVersion`, and `PageSize` are populated, mirroring the assertions in
  `TestDirectLifecycleDiagnosticsAndDMLPlanDoNotExecute`
  (`integration/lifecycle_test.go:234`).
- `TestIntrospectionPlanReturnsSelectPlan`: `Plan` for
  `SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?` returns a non-empty plan.
- `TestIntrospectionPlanForDMLDoesNotExecuteImplicitly`: two pools on one
  fixture. Reads the sentinel row from the first pool, calls `Plan` with
  `UPDATE GO_COUNTRY SET COUNTRY = 'changed' WHERE ID = 1` and with
  `DELETE FROM GO_COUNTRY WHERE ID = 1`, then asserts on row state, not on
  absence of error: the same `*sql.Conn` still reads `COUNTRY = 'USA'` for
  `ID = 1`, the row count for `ID = 1` is still 1, and the second pool — a
  separate native attachment — reads the same values. The plan strings are only
  logged, because an empty DML plan is permitted.

  This case alone is not sufficient evidence. With no explicit transaction the
  native prepare transaction is read-only, so a hypothetical execute would be
  refused by the engine and the assertions would pass for the wrong reason. It
  establishes only that the read-only default path is safe; the next test
  removes that escape.
- `TestIntrospectionPlanInsideWritableTransaction`: one test carrying both the
  transaction-selection check and the load-bearing safety check, because both
  need the same writable explicit transaction. On a single `*sql.Conn` from the
  first pool:

  1. `conn.BeginTx(ctx, nil)` — a writable read-committed transaction.
  2. Inside it, `ExecContext` a `CREATE TABLE GO_INTROSPECTION_TXPROBE (ID
     INTEGER NOT NULL PRIMARY KEY, TEXT_VALUE VARCHAR(32))` and an `INSERT`
     of `(1, 'sentinel')`. InterBase metadata is transactional, so neither the
     table nor the row is visible outside this transaction yet.
  3. Through `Raw` on the same `*sql.Conn`, `Plan` a
     `SELECT TEXT_VALUE FROM GO_INTROSPECTION_TXPROBE WHERE ID = 1`. It must
     succeed with a non-empty plan. **This is the transaction-selection proof:**
     if `Plan` had started its own implicit transaction, the uncommitted
     metadata would be invisible and prepare would fail with an unknown-table
     error.
  4. Still inside the writable transaction, `Plan` each mutating statement form
     in turn, reading state back on the same `*sql.Conn` after each one:
     `UPDATE GO_INTROSPECTION_TXPROBE SET TEXT_VALUE = 'changed' WHERE ID = 1`,
     `DELETE FROM GO_INTROSPECTION_TXPROBE WHERE ID = 1`, and
     `INSERT INTO GO_INTROSPECTION_TXPROBE VALUES (2, 'inserted')`. After all
     three, assert `TEXT_VALUE` for `ID = 1` is still `'sentinel'` and that
     exactly one row remains. **This is the safety proof:** the transaction the
     DML would have run in is writable and the read sees its own uncommitted
     work, so an execute would be both permitted and visible.

     Then cover the procedure form, which is the statement type most likely to
     behave differently: inside the same transaction, create
     `GO_INTROSPECTION_TXPROC` as a procedure whose body inserts
     `(3, 'procedure')` into the probe table, `Plan`
     `EXECUTE PROCEDURE GO_INTROSPECTION_TXPROC`, and assert the row count is
     still exactly one. This case is called out separately because the
     execution path does treat procedures specially — `ib_connection_query`
     re-prepares an implicit procedure under a write transaction
     (`native.c:7486`) — while `ib_statement_prepare_mode` has no such branch.
     The spec claims prepare-only for every statement type, so the procedure
     form must be demonstrated rather than assumed.
  5. `tx.Rollback()` must return nil rather than `sql.ErrTxDone`; a
     `sql.ErrTxDone` here would mean `Plan` had already completed the caller's
     transaction and the later assertions would pass vacuously. Then assert from
     the second pool that `GO_INTROSPECTION_TXPROBE` does not exist — together
     these prove `Plan` neither committed nor rolled back the caller's
     transaction at any point.

  An earlier draft of this test used `SELECT ... FOR UPDATE` as the
  transaction-selection signal, on the assumption that it only prepares inside a
  writable transaction. That assumption is wrong for the prepare path: the
  `"SELECT FOR UPDATE requires an explicit writable transaction"` check exists
  only in the execution paths (`native.c:7504` for direct query,
  `native.c:8171` for prepared query). `ib_statement_prepare_mode` treats
  `isc_info_sql_stmt_select_for_upd` purely as an output-describe case
  (`native.c:7913`) and never inspects `transaction_read_only`. The uncommitted
  metadata probe is used instead because it depends on visibility, which the
  prepare path demonstrably does exercise.
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
