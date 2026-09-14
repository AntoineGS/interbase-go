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

`make build` creates `bin/ibprobe`. `make test` runs Go tests and five native
AddressSanitizer harnesses, including leak detection. Without the environment
variables below, live Go tests explicitly skip. Native harnesses use synthetic
descriptors/status vectors and injected lifecycle failures, and do not attach
to a database.

The disposable write/transaction/type contracts are separate from the read-only
probe. Run them with an existing local server image:

```sh
IMAGE=your-local-interbase-image make test-integration-docker
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

The probe checks native attachment, Dialect 1 double-quoted string literals,
positional parameter binding, UDF argument metadata, and early cursor close
followed by connection reuse. It runs only fixed SELECTs. It does not invoke
UDFs, create a test database, or execute DDL/DML.

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

## Supported Boundary

- Client SQL Dialect 1. `Config.Charset` defaults to UTF8; WIN1250 is also
  supported. Go strings and SQL text remain UTF8 at the public boundary.
- Direct `QueryContext`/`QueryRowContext` and `PingContext`.
- `ExecContext` for DML/DDL, with DML affected-row counts and implicit commit.
  `LastInsertId` is unsupported.
- Reusable server-side prepared statements via `PrepareContext`.
- Explicit read-committed transactions, read-only transactions and SQL
  savepoints. Use the Go transaction methods for commit/rollback; full SQL
  transaction-control statements are rejected.
- Positional parameters: `string`, `[]byte`, `int64`, `float64`, `bool`,
  `time.Time`, and `nil`, plus standard `database/sql` numeric and Valuer
  conversions.
- CHAR/VARCHAR, SMALLINT/INTEGER/INT64, FLOAT/DOUBLE, BOOLEAN, and NULL results.
- Scaled integer results as exact decimal strings rather than lossy floats.
- Wide Dialect-1 numerics as approximate floating-point values.
- Materialized text and binary BLOBs, including empty versus NULL values,
  segmented transfer, and text-column charset conversion.
- Timestamp values as `time.Time`, preserving wall-clock fields and attaching
  UTC as a convention, not claiming the stored value has a time zone.
  Date/time decoding exists but Dialect 1 may reject those distinct SQL types.
- One active cursor per native connection; `database/sql` may pool separate
  connections. Always close rows or consume them to EOF.

Each query cursor borrows an explicit connection transaction or owns a
read-committed, read-only implicit transaction that is rolled back on
close/EOF/error. Prepared statement handles remain allocated across executions
and are released by `Stmt.Close` or connection cleanup; cursor cleanup never
drops the reusable handle. The connection serializes native access, and
transaction-start/cleanup failures invalidate it. A Go `driver.Validator`
prevents broken attachments from returning to the pool. Result descriptors
always request NULL indicators, including expressions described as
non-nullable.

The server-reported statement type must be plain SELECT before query execution.
Prepared statements retain the server-side statement handle and reset their
input/output SQLDA storage for each execution. Prepared writes use the explicit
connection transaction when present; otherwise each successful write commits
its implicit transaction and each failed write rolls it back. SELECT permission
and a read-only transaction do not sandbox side effects inside external UDFs;
do not run untrusted SQL. The supplied probe/tests never call them.

## Known Limits

- No arrays or standalone QUADs. BLOBs are materialized rather than streamed,
  with a 64 MiB read limit; subtype and expression metadata limitations still
  apply. Unsupported descriptors fail explicitly.
- No named parameters, services, events, or distributed transactions.
- Mixed-character-set casts may require an explicit source-charset cast on
  the parameter. For a UTF8 attachment, declare `CAST(? AS VARCHAR(40)
  CHARACTER SET UTF8)` before converting to WIN1250 and back. The original
  WIN1250-inferred parameter failed in a standalone C reproduction on the
  tested InterBase 15.1 stack too. See the native comparison in
  [integration/README.md](integration/README.md#mixed-character-set-parameters).
  The driver does not silently rewrite SQL or open secondary attachments.
- **Context cancellation cannot interrupt an in-flight native call.** It is
  checked before/after native operations and between rows. A stalled native
  call can outlive its context deadline; use a process timeout for experiments.
- No TLS configuration API or validated cross-version/platform matrix yet.
  The probe must only be used on a trusted network unless native transport
  security is independently configured and verified.
- Error reporting uses bounded SQLCODE messages plus a numeric native status.
  It deliberately avoids the legacy unbounded `isc_interprete` function and
  omits detailed server-provided strings.
- Race/ASan checks do not establish complete leak-freedom inside the proprietary
  client library, safe asynchronous cancellation, or production readiness.

## Verification Record

On 2026-09-13, using the isolated InterBase 15.1 Docker server and its matching
client, the expanded integration suite had **54 passing top-level groups,
zero failures and zero skips**. Writes, transactions, native statement reuse,
CHAR padding, wide numerics, and BLOB regressions passed. The faithful upstream
charset test also passed: a five-field row inserted through UTF8 was read back
identically through both UTF8 and WIN1250 attachments. The additional
Go-specific nested-cast test now explicitly declares its UTF8 input, following
a standalone native API comparison; it was a test SQL correction, not a new
driver binding fix. Offline tests, all
five ASan/leak-checking native harnesses, race tests, vet, and build passed.

The following record describes the earlier read-only baseline, not the current
full integration suite:

On 2026-09-11, using the installed InterBase 2020 Linux client/SDK:

- The executable probe connected directly to the reference database and read
  **282 UDF argument catalog rows**, without invoking any UDFs.
- All current Go tests, including the live integration tests, passed.
- Native status, descriptor/value, and lifecycle-failure harnesses passed
  AddressSanitizer with leak detection enabled. The lifecycle harness also
  checks read-only transaction flags and plain-SELECT gating without executing
  statements on a server.

After the successful live run, review added pool validation and more defensive
cleanup-failure handling. The final offline build, tests, race detector, native
harnesses, and `go vet` passed. The final live repeat timed out inside attachment;
a separate TCP check showed connections to the database endpoint stuck in
SYN-SENT. Those last lifecycle changes therefore still need a live repeat when
the endpoint is reachable. The successful earlier live run is not being used
as evidence of a successful final rerun.

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
