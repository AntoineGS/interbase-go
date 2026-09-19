# interbase-go

**Experimental Linux/amd64 proof of concept, not a production driver.**

A small Go `database/sql` connector using the official InterBase native client
through cgo. No Python runtime, ODBC layer, Firebird driver, or wire-protocol
implementation is involved. The module name `interbase-go` is local/provisional.

## Requirements

- Go 1.23 or newer, GCC/Clang, Linux x86-64, and `CGO_ENABLED=1`.
- Official InterBase SDK headers at `/opt/interbase/include`.
- Official InterBase client library at `/opt/interbase/lib/libgds.so`.
- Network access and credentials for an existing InterBase database.

The current cgo flags use that installation prefix and embed its library path
as a runtime search path. Other platforms and installation layouts have not
been validated. Vendor headers, binaries, and licenses are not redistributed
in this project.

## Build and Test

```sh
make build
make test
```

`make build` creates `bin/ibprobe`. `make test` runs Go tests and ten native
AddressSanitizer harnesses, including leak detection. Without the environment
variables below, live Go tests explicitly skip. Native harnesses use synthetic
descriptors/status vectors and injected lifecycle failures, and do not attach
to a database.

For a non-default SDK layout, override the paths used by both Makefile targets:

```sh
make test INTERBASE_INCLUDE=/path/to/include INTERBASE_LIB=/path/to/lib
```

The disposable write/transaction/type contracts are separate from the read-only
probe. Run them with an existing local server image:

```sh
IMAGE=your-local-interbase-image INTERBASE_INCLUDE=/path/to/include make test-integration-docker
```

See [integration/README.md](integration/README.md) for prerequisites, isolation,
runner tests, and mixed-character-set parameter guidance. The runner copies
client executables from the selected image onto the host, so use a trusted image.

## Live Probe

Set these variables using your preferred secure environment/credential tooling:

| Variable | Meaning |
| --- | --- |
| `INTERBASE_DATABASE` | Native attachment string, such as `host/3050:/path/database.ib` or `host/3050:C:\data\database.ib` |
| `INTERBASE_USER` | Database account; prefer a dedicated read-only account |
| `INTERBASE_PASSWORD` | Database password; must be set, even if intentionally empty |

No credentials are embedded in the source or loaded automatically from MCP.
The probe prints pass/fail summaries and a UDF argument count, not private
catalog names or connection settings.

```sh
timeout 30s go run ./cmd/ibprobe
go test -v -run '^TestLive' -count=1 -timeout=60s .
```

The probe checks a Dialect 1 native attachment, Dialect 1 double-quoted string
literals, positional parameter binding, UDF argument metadata, and early
cursor close followed by connection reuse. It runs only fixed SELECTs. It does
not invoke UDFs, create a test database, or execute DDL/DML.

The live tests also cover UTF-8 parameters, empty strings versus NULL,
fixed-width padding, integers, doubles, timestamps with fractional seconds,
repeated early cursor closure, query-error recovery, and cancellation between
native calls. No fixture writes are required.

## Go Usage

```go
connector, err := interbase.NewConnector(interbase.Config{
    Database: os.Getenv("INTERBASE_DATABASE"),
    User:     os.Getenv("INTERBASE_USER"),
    Password: os.Getenv("INTERBASE_PASSWORD"),
    Dialect:  3,
})
if err != nil {
    return err
}
db := sql.OpenDB(connector)
defer db.Close()
db.SetMaxOpenConns(1)

var name string
err = db.QueryRowContext(ctx,
    "SELECT RDB$RELATION_NAME FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = ?",
    "RDB$DATABASE",
).Scan(&name)
```

Import this module as `interbase "interbase-go"`. It intentionally has no
registered DSN driver: use `NewConnector` and `sql.OpenDB`.

`Config.Dialect` is an `int`: zero or omission selects Dialect 3; explicit
values 1 and 3 are supported, and every other value fails before attachment.
Set `Dialect: 1` to opt into Dialect 1. The example spells out the Dialect 3
default; it requires no dependency beyond Go's standard `database/sql` package
and this connector.

Set `Config.Host` together with `Config.Database` to construct a native
`[host[/port]]:database` attachment. `Config.TLS` adds the InterBase native
TLS attachment parameters, while `Config.Role`, `Config.EncryptedPassword`,
and `Config.SystemEncryptionPassword` populate the corresponding attachment
parameters. These values are validated before the native client is called and
are redacted from returned errors.

> **TLS security warning:** with vendor client `LI-V15.1.0.42`, a trusted CA
> does **not** establish server identity. The client accepted an intentionally
> wrong DNS hostname in the isolated strict-TLS matrix, including through the
> vendor `isql`. Do not use `TLS.Enabled` or CA trust alone as proof of hostname
> verification. This project deliberately has no Go-side TLS preflight
> workaround: a second connection would not authenticate the actual native
> InterBase attachment.

`Config.TransactionOptions` supplies connector-wide native defaults for
`NoWait`, `NoRecordVersion`, and table reservations. Per-transaction
`database/sql.TxOptions` selects read-only access and the standard
read-committed, repeatable-read/snapshot, or serializable isolation level.

### Explicit direct API

Use `Open` when the application needs an explicitly owned attachment and the
bounded cursor/transaction operations that are not expressible through
`database/sql`:

```go
attachment, err := interbase.Open(ctx, interbase.Config{
    Database: os.Getenv("INTERBASE_DATABASE"),
    User:     os.Getenv("INTERBASE_USER"),
    Password: os.Getenv("INTERBASE_PASSWORD"),
    Dialect:  3,
})
if err != nil {
    return err
}
defer attachment.Close()

tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{
    Isolation: sql.LevelReadCommitted,
})
if err != nil {
    return err
}

cursor, err := tx.Query(ctx,
    "SELECT ID, COUNTRY FROM GO_COUNTRY WHERE ID = ? FOR UPDATE", int64(1),
)
if err != nil {
    _ = tx.Rollback()
    return err
}
defer cursor.Close()

if err := cursor.SetName("country_cursor"); err != nil {
    _ = tx.Rollback()
    return err
}
for {
    hasRow, err := cursor.Next(ctx)
    if err != nil {
        _ = tx.Rollback()
        return err
    }
    if !hasRow {
        break
    }
    cells, err := cursor.Row()
    if err != nil {
        _ = tx.Rollback()
        return err
    }
    _ = cells // Cell.Value and Cell.Indicator are Go-owned snapshots.
}
return tx.Commit()
```

### Distributed direct transactions

Use `BeginDistributed` when one atomic native transaction must cover multiple
explicit attachments. The engine receives all participants in one
`isc_start_multiple` call; prepare and completion must be performed through the
coordinator, not through an individual participant transaction:

```go
coordinator, err := interbase.BeginDistributed(ctx, []interbase.Participant{
    {Attachment: firstAttachment, Options: interbase.TxOptions{Isolation: sql.LevelReadCommitted}},
    {Attachment: secondAttachment, Options: interbase.TxOptions{Isolation: sql.LevelReadCommitted}},
})
if err != nil {
    return err
}
left, err := coordinator.Participant(0)
if err != nil {
    _ = coordinator.Rollback(context.Background())
    return err
}
right, err := coordinator.Participant(1)
if err != nil {
    _ = coordinator.Rollback(context.Background())
    return err
}
if _, err := left.Exec(ctx, "UPDATE FIRST_TABLE SET VALUE = ?", int64(1)); err != nil {
    _ = coordinator.Rollback(context.Background())
    return err
}
if _, err := right.Exec(ctx, "UPDATE SECOND_TABLE SET VALUE = ?", int64(1)); err != nil {
    _ = coordinator.Rollback(context.Background())
    return err
}
if err := coordinator.Prepare(ctx, []byte("application-recovery-key")); err != nil {
    _ = coordinator.Rollback(context.Background())
    return err
}
return coordinator.Commit(ctx)
```

`Prepare` calls the native prepare API with the optional recovery message.
After prepare or an ambiguous completion error, resolve the coordinator
explicitly; `Attachment.Close` refuses to detach an unresolved distributed
participant. `RecoveryInfo` returns copied database and transaction identifiers
for every participant and remains available while the outcome is uncertain. If
the native client consumed the coordinator while reporting that error,
`Commit` and `Rollback` return `ErrDistributedNativeUnavailable`; resolve the
copied identifiers through Services before releasing local ownership.

### Creating and dropping an owned database

Use `CreateDatabase` only with a path owned by the current operation. It calls
the native InterBase create API, never overwrites an existing database, and
returns an attachment that can be explicitly dropped:

```go
// Imports used below: context, errors, and filepath.
attachment, createErr := interbase.CreateDatabase(ctx, interbase.Config{
    Database: filepath.Join(tempDir, "database.ib"),
    User:     "SYSDBA",
    Password: password,
    Dialect:  3,
}, interbase.CreateOptions{PageSize: 4096})
if createErr != nil {
    // A successful native create followed by setup or context failure can
    // return both an attachment and an error. Clean it up before returning;
    // use a non-canceled context so cleanup is still attempted.
    if attachment == nil {
        return createErr
    }
    if dropErr := attachment.DropDatabase(context.Background()); dropErr != nil {
        return errors.Join(createErr, dropErr, attachment.Close())
    }
    return createErr
}

// Finish every transaction and close every cursor before dropping.
if err := attachment.DropDatabase(ctx); err != nil {
    // ErrAttachmentBusy and non-consuming native drop errors preserve the
    // attachment for retry. A consumed-handle diagnostic such as status
    // 335544667 closes the attachment despite returning an error; do not retry.
    return err
}
return attachment.Close() // idempotent after a successful drop
```

`Close` detaches and never drops a database. A failed or canceled create may
return a non-nil attachment with an error; retain it and either finish the
operation or call `DropDatabase` explicitly. Do not call `DropDatabase` on a
`database/sql` pool or an application database. Every dedicated `Attachment`
is eligible because `DropDatabase` is an explicit destructive caller
authorization; only fixture paths owned by the current operation should be
passed to it.

`Attachment.Diagnostics` combines the linked client version, server version,
ODS version, page size, SQL dialect, and read-only state. `DatabaseInfo` returns
copied `InfoItem` payloads; use `Uint64`, `Int64`, `Bool`, or UTF-8 `Text` for
bounded decoding. `Transaction.Plan` only prepares a statement: a valid DML
statement may return an empty plan, which never means that the DML executed.

`Attachment` may own multiple independent direct transactions; each transaction
has its own native handle and may own multiple cursors. Normal commit/rollback
closes those cursors, while
`CommitRetaining`/`RollbackRetaining` keep the transaction and active cursors
usable. `Cursor.SetName` is one-shot and requires a writable transaction, making
`UPDATE ... WHERE CURRENT OF country_cursor` possible. `Transaction.Plan`,
`Attachment.DatabaseInfo`, and `Transaction.Info` return bounded copied data in
`Plan`/`InfoItem` values. `Cell.Value` follows the existing scalar conversion
rules, copies byte slices, and reports `SQLIND_NULL` as bit 15 while preserving
positive change-indicator flags. Direct ChangeView reads use a serializable
transaction: execute `SET SUBSCRIPTION ... ACTIVE` on that transaction before
querying it. The `database/sql` path rejects subscription activation because
pooled session state cannot safely represent it. Close the attachment when
finished; it rolls back any still-active direct transaction.

Direct array parameters and results use `Array`, whose inclusive bounds are
kept separately from its flat `Elements` slice. Elements use rightmost
dimension-fastest ordering. Bounds and element types are validated against the
server descriptor before an array is sent. Array values are intentionally
available only through this direct API; `database/sql` rejects array
parameters and results instead of silently materializing them.

Direct BLOB results are returned as an opaque, transaction-generation-bound
`BlobRef`. Pass that reference to `Transaction.OpenBlob` for an
`io.ReadCloser`, or use `Transaction.CreateBlob` to stream an `io.Reader` into
a new BLOB. A reference can also be used as a direct BLOB parameter in the
same transaction. References and open streams are owned by the active
transaction and become unusable when it completes; cursor, transaction, and
attachment cleanup closes any remaining native BLOB handles. The standard
`database/sql` path continues to materialize text and binary BLOBs.

## Supported Boundary

- Client SQL Dialect 3 by default, or Dialect 1 when selected with
  `Config.Dialect`. `Config.Charset` defaults to UTF8; UTF8, WIN1250, WIN1252,
  ISO8859_1, and ASCII are supported. Go strings and SQL text remain UTF8 at
  the public boundary.
- Structured native attachments with host/port, roles, alternate encrypted and
  system-encryption credentials, and TLS parameters.
- Direct `QueryContext`/`QueryRowContext` and `PingContext`.
- Explicit `Open` attachments with owned transactions/cursors, retaining
  completion, positioned cursor names, plans, raw info items, SQLDA
  change-indicator access, and serializable ChangeView reads.
- `ExecContext` for DML/DDL, with DML affected-row counts and implicit commit.
  `LastInsertId` is unsupported.
- Reusable server-side prepared statements via `PrepareContext`.
- Best-effort context cancellation for DSQL prepare, execute, and fetch calls:
  `database/sql` `PrepareContext`, direct and prepared `ExecContext`,
  `QueryContext`/`QueryRowContext` and `Rows.Next`, plus direct
  `Transaction.Query`, `Transaction.Exec`, `Transaction.Plan`, and
  `Cursor.Next`. The executing native result remains authoritative; the
  cancellation request never replaces a result that already completed.
- Explicit transactions with standard read-committed, repeatable-read/snapshot,
  and serializable isolation, read-only transactions, connector-level wait and
  record-version policies, table reservations, and SQL savepoints. Use the Go
  transaction methods for commit/rollback; full SQL transaction-control
  statements are rejected.
- Positional parameters: `string`, `[]byte`, `int64`, `float64`, `bool`,
  `time.Time`, and `nil`, plus standard `database/sql` numeric and Valuer
  conversions. Direct `Query`/`Exec` additionally accept validated `Array`
  values.
- CHAR/VARCHAR, OCTETS CHAR/VARCHAR, SMALLINT/INTEGER/INT64, FLOAT/DOUBLE,
  BOOLEAN, DATE/TIME/TIMESTAMP, and NULL results.
- OCTETS CHAR/VARCHAR as raw `[]byte`; fixed CHAR values retain space padding,
  and empty values remain distinct from NULL.
- Dialect 3 scaled NUMERIC/DECIMAL values as exact decimal strings, including
  negative values. Checked binding enforces declared precision and native range,
  rejects scientific notation and excess nonzero fractional digits, and accepts
  trailing zeroes. Zero-scale NUMERIC/DECIMAL values scan as `int64`.
- Dialect 1 preserves server-side conversion for numeric text arguments.
  Scaled integer results remain exact decimal strings; wide Dialect-1 numerics
  remain approximate floating-point values.
- Materialized text and binary BLOBs, including empty versus NULL values,
  segmented transfer, and text-column charset conversion. The direct API also
  exposes bounded streaming reads and writes through `BlobRef`, `OpenBlob`,
  and `CreateBlob`.
- Explicitly owned asynchronous database event subscriptions through the
  [`events`](events) package. Counts accumulate across native callbacks,
  cancellation only cancels the current wait, and close waits for callbacks
  before detaching. Server/native-client timing and grouping remain observable
  behavior; the isolated integration fixture verifies commit/rollback timing,
  while exactly-once delivery and row-level identity are not promised.
- A bounded typed Services Manager package for server information, logs,
  statistics, logical backup/restore/dump, archive/tablespace operations,
  database maintenance and properties, users, aliases, and limbo resolution.
  See [services/README.md](services/README.md). It uses the official native
  client and does not accept arbitrary Services parameter blocks.
- DATE/TIME/TIMESTAMP values as `time.Time`, preserving wall-clock fields and
  attaching UTC as a convention, not claiming stored timezone data. DATE uses
  midnight; TIME uses a `1900-01-01` anchor and accepts Go's year-zero values
  from `time.Parse`; InterBase precision is 100 microseconds.
- Standard `Rows.ColumnTypes` metadata: database type, scan type, length,
  nullability, and decimal precision/scale are immutable execution snapshots.
  Properties unavailable from the result are reported unknown; expressions do
  not promise precision or nullability that cannot be established.
- `EXECUTE PROCEDURE` through `QueryContext`/`QueryRowContext` when it returns
  output (exactly one row), or `ExecContext` when it has no output. `ExecContext`
  rejects output-producing procedures. An implicit procedure query commits its
  write transaction after EOF or early close, and rolls back on execution,
  cancellation, or completion failure; explicit transactions remain caller-owned.
 - Multiple direct cursors may remain active on one native connection, and
  `ExecContext` may run while those rows are open. A prepared statement still
  permits only one active execution at a time; `database/sql` may pool separate
  connections. Always close rows or consume them to EOF.
  - Explicit direct attachments may own multiple independent direct transactions.
    `BeginDistributed` provides one native two-phase transaction across multiple
    attachments. Persist `DistributedTransaction.RecoveryInfo` before ambiguous
    completion; its copied database IDs and transaction IDs are in participant
    input order for external Services `ListLimbo`/commit/rollback recovery.
    `ReleaseAfterRecovery` and `Abandon` only relinquish local ownership and do
    not resolve limbo. The live client-death test syncs and closes its private
    recovery and post-`ListLimbo` decision files, but does not test server
    restart or power loss and makes no power-loss durability claim.

Each ordinary SELECT cursor borrows an explicit connection transaction or owns
a read-committed, read-only implicit transaction that is rolled back on
close/EOF/error. Prepared statement handles remain allocated across executions
and are released by `Stmt.Close` or connection cleanup; cursor cleanup never
drops the reusable handle. The connection serializes native access, and
transaction-start/cleanup failures invalidate it. A Go `driver.Validator`
prevents broken attachments from returning to the pool. Result descriptors
always request NULL indicators, including expressions described as non-nullable.

The server-reported statement type must be an allowed result-producing SELECT
(plain SELECT, SELECT FOR UPDATE inside an explicit writable transaction, or an
output-producing procedure) before ordinary query execution.
Prepared statements retain the server-side statement handle and reset their
input/output SQLDA storage for each execution. Prepared writes use the explicit
connection transaction when present; otherwise each successful write commits
its implicit transaction and each failed write rolls it back. SELECT permission
and a read-only transaction do not sandbox side effects inside external UDFs;
do not run untrusted SQL. The supplied probe/tests never call them.

## Known Limits

- `database/sql` does not expose arrays or standalone QUADs. The direct API
  supports bounded arrays and `io.Reader`/`io.ReadCloser` BLOB streaming, but
  does not yet provide Python-style BLOB seeking, `readline`, or alternate
  subtype behavior. `database/sql` BLOB results remain materialized with a
  64 MiB read limit.
  Unsupported descriptors fail explicitly.
- No named parameters. Raw/custom TPBs and lock timeouts are not exposed;
  distributed transaction failure-injection and live limbo recovery require a
  server fixture with an available Services Manager.
- Mixed-character-set casts may require an explicit source-charset cast on
  the parameter. For a UTF8 attachment, declare `CAST(? AS VARCHAR(40)
  CHARACTER SET UTF8)` before converting to WIN1250 and back. The original
  WIN1250-inferred parameter failed in a standalone C reproduction on the
  tested InterBase 15.1 stack too. See the native comparison in
  [integration/README.md](integration/README.md#mixed-character-set-parameters).
  The driver does not silently rewrite SQL or open secondary attachments.
- **DSQL context cancellation is best effort, not a hard deadline.** The DSQL
  prepare/execute/fetch operations listed under [Supported Boundary](#supported-boundary)
  start a native cancellation request when their context is canceled
  after native entry. The executing result is authoritative: a successful
  prepare, execute, or fetch remains successful even if the context expires
  concurrently, while an executing `isc_cancelled` result is reported through
  a typed cancellation error. A successful cancellation request by itself does
  not establish the operation outcome. Each fetch watcher is joined before the
  rows, cursor, or statement can be closed or reused.
- Cancellation latency depends on the native client and operation. On the
  tested Linux/amd64 InterBase 15.1 `LI-V15.1.0.42` stack, a roughly
  3.2-second aggregate fetch returned `isc_cancelled` about 101 ms after the
  request, while a statement waiting for a row lock returned only after roughly
  9.2–10 seconds. These are observations, not guarantees; a stalled DSQL call
  can outlive its context deadline, so use process supervision for a hard bound.
- Canceled DSQL errors match the originating context through `errors.Is` and
  retain native diagnostics through `errors.As`. A canceled mutating operation
  returns `CancellationError` when rollback or the caller-owned transaction
  state is confirmed. If the final write outcome cannot be established,
  `UncertainOutcomeError` is returned instead. Canceled writes must not be
  blindly retried: reconcile an uncertain outcome using an operation ID or
  database state before deciding whether a retry is safe. Cancellation alone
  never becomes `driver.ErrBadConn`, so `database/sql` does not replay it.
  Cancellation never commits or rolls back a caller-owned explicit transaction;
  the caller remains responsible for completing it.

  ```go
  ctx, cancel := context.WithTimeout(context.Background(), time.Second)
  defer cancel()

  _, err := db.ExecContext(ctx, query, args...)
  if err == nil {
      return nil
  }

  var uncertain *interbase.UncertainOutcomeError
  if errors.As(err, &uncertain) {
      // Reconcile the operation ID or database state before any retry.
      return err
  }

  var canceled *interbase.CancellationError
  if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
      if errors.As(err, &canceled) {
          var native *interbase.NativeError
          _ = errors.As(err, &native) // isc_cancelled diagnostics, when present.
          return err
      }
  }
  return err
  ```
- Context cancellation does not interrupt non-DSQL native calls once they have
  entered the client. Attachment open/create/drop, transaction begin/commit/
  rollback/retaining and distributed completion, BLOB segment/array I/O,
  Services, and Events keep their existing admission and cleanup semantics.
  Event wait cancellation remains a separate subscription behavior and is not
  a DSQL cancellation guarantee.
- TLS is passed to the native client as an attachment option. The repository
  has an isolated certificate fixture, but the strict matrix is intentionally
  nonzero with vendor client `LI-V15.1.0.42`: it accepts a wrong hostname.
  Correct trust, unrelated-CA rejection, and required-TLS/no-plaintext-fallback
  checks pass, but CA trust is not server identity verification. There is no
  validated cross-version/platform matrix and no unsafe Go-side preflight
  workaround. Use the probe only on a trusted network unless transport security
  is independently configured and verified for the deployed native client.
- Error reporting uses bounded SQLCODE messages plus a numeric native status.
  It deliberately avoids the legacy unbounded `isc_interprete` function and
  omits detailed server-provided strings.
- Race/ASan checks do not establish complete leak-freedom inside the proprietary
  client library, a universal DSQL cancellation latency/thread-safety guarantee,
  or production readiness.

## Verification Record

Production-hardening commands and execution semantics are documented in
[docs/production-hardening.md](docs/production-hardening.md). Deferred feature
requests are tracked in [docs/TODO.md](docs/TODO.md).

On 2026-09-18, the complete serial DSQL-cancellation verification passed with
the Linux/amd64 InterBase 15.1 client: `make test` (eight Go packages and ten
native ASan/leak harnesses) took 19.99 seconds, race took 28.90 seconds,
checkptr 3.38 seconds, tagged integration vet 1.34 seconds, and the runner
reported **42/42** in 11.09 seconds. The cancellation target passed ten
iterations in 12.88 seconds (16.68 seconds including Docker setup/cleanup),
the fault matrix took 16.72 seconds, and the native lifecycle target took 6.80
seconds. The final four-worker, 120-second soak ran for 2m0.003813076s and
validated 122,283 operations: 61,143 committed writes, 61,140 rolled-back
writes, 61,143 persisted values, 30,573 physical opens/closes, and 30,572
identity replacements. Throughput was 1,018.99 operations/s; average and
maximum latencies were 3.925199 ms and 27.478933 ms. After close, both pools
were empty, with six file descriptors and two goroutines. `make build` took
0.37 seconds. The raw soak log is
`/tmp/opencode/dsql-context-task-7-20260918/09-test-soak-120s.log`.

The initial unqualified `make test`, race, and vet invocations failed before
testing because the temporary SDK include/runtime paths were not supplied;
reruns with the explicit paths in
[production-hardening.md](docs/production-hardening.md) passed. These results
measure a two-minute soak only and do not add a cross-version latency or
thread-safety guarantee.

On 2026-09-16, a fresh controller run completed `make test` with the official
SDK: all six Go packages and all ten native ASan/leak harnesses passed.
`make test-runner BATS=/tmp/opencode/parity-bats/bin/bats` reported **21/21**
passing tests, `go vet -tags=integration ./...` passed, and the ordinary full
Docker integration runner passed, including its Services framing precheck.
The ordinary runner deliberately excludes the strict TLS matrix because its
known hostname-verification defect makes that security suite return nonzero.

Fresh race, checkptr, build, ShellCheck, and all root `TestLive*` checks also
passed. The strict isolated TLS rerun returned 12 passing checks and four
wrong-hostname failures, with its disposable container and keys cleaned up.
The race command requires a runtime library path when cgo is linked to an
explicit SDK directory. For the temporary SDK used in this verification:

```sh
LD_LIBRARY_PATH=/tmp/opencode \
  CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
  CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
  go test -race ./... -count=1 -timeout=120s
```

Prior migration evidence is historical context only, not verification of the
current source tree.

This proves the basic native-client approach is viable. It does not add support
to sqls; catalog caching, InterBase parsing, UDF completion/hover/signatures,
and driver hardening remain separate work.

## Reference

The local `../InterBasePython` checkout and the public
[Embarcadero InterBasePython](https://github.com/Embarcadero/InterBasePython)
implementation were used to compare native API usage and NULL handling. The
installed InterBase SDK headers define the ABI; Firebird headers are not a
substitute. This is an independent, limited implementation, not a wholesale
translation of the Python package.
