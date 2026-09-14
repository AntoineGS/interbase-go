# Python-Derived Database Contracts

This is a test-first migration of selected behavior from
[Embarcadero/InterBasePython](https://github.com/Embarcadero/InterBasePython),
source commit `f2dcefccd1a3fcf0f06d72849a615aba4214106c`.
It targets the Go `database/sql` API, not a Python cursor compatibility layer.

There are 44 contract test functions, a live fixture smoke test, and offline
helper tests. Table-driven subtests cover additional cases, including each
VARCHAR parameter length from 0 through 254. These counts are Go test groups,
not a claim that 44 entire Python test methods have been ported unchanged.

## Run

From the repository/worktree root:

```sh
# Compile all contracts without provisioning a database.
go test -tags=integration ./integration -run '^$'

# Run integration-helper regressions without a database server.
go test -tags=integration ./integration -run '^(TestPingDatabase|TestCleanupDatabase)' -count=1

# First check local provisioning, attachment, querying, and cleanup.
go test -tags=integration ./integration -run '^TestReadFixtureSmoke$' -v -count=1 -timeout=2m

# Read-side contracts, including regressions that may expose driver bugs.
go test -tags=integration ./integration -run '^TestRead' -v -count=1 -timeout=5m

# Full contract suite, including unfinished write/transaction capabilities.
go test -tags=integration ./integration -v -count=1 -timeout=10m
```

The `integration` build tag is the opt-in to create and delete local disposable
databases. No separate provisioning command or opt-in environment variable is
needed. Ordinary `go test ./...` does not include this package.

The existing root-level `TestLive*` tests are unchanged and have their own
`INTERBASE_DATABASE` opt-in. To run the entire offline baseline regardless of
your application environment:

```sh
env -u INTERBASE_DATABASE -u INTERBASE_USER -u INTERBASE_PASSWORD make test
env -u INTERBASE_DATABASE -u INTERBASE_USER -u INTERBASE_PASSWORD go test -race ./... -count=1 -timeout=60s
go vet -tags=integration ./...
```

## Requirements And Defaults

- Linux/amd64, Go 1.23+, CGO and the official InterBase SDK/client, as required by
  the existing driver.
- A working **local InterBase server**, with a valid license and permission to
  create databases in the suite-owned temporary directories. Client tools alone
  are insufficient; the harness does not install, start, or reconfigure services.
- Default executable: `/opt/interbase/bin/isql`.
- Default test credentials: `SYSDBA` / `masterkey`.
- Client/database SQL Dialect 1 and UTF8, matching the current connector.

Optional overrides are `INTERBASE_TEST_ISQL`, `INTERBASE_TEST_USER`, and
`INTERBASE_TEST_PASSWORD`. An unset password selects `masterkey`; an explicitly
empty password remains empty. Defaults are confined to this test helper, not
the driver or CLI. Use your normal secure environment tooling for custom
credentials; no credential files are loaded automatically.

The temporary root is the platform temporary directory (`TMPDIR` when set).
It must be an absolute local path without colons or control characters. The
harness creates a private, unique `interbase-go-fixture-*` directory and a new
`database.ib` inside it. It accepts **no existing database target or remote
attachment string**, and ignores the application's `INTERBASE_DATABASE`,
`INTERBASE_USER`, and `INTERBASE_PASSWORD` settings.

## Fixture Lifecycle

`internal/testfixture` runs InterBase `isql` directly, with SQL on standard
input and no shell or credential command-line arguments. It creates the small
schema in `testdata/schema.sql`, independent of the driver's unfinished write
support. It does not run the original fixture's security-user or encryption
administration statements.

Each top-level database test gets a fresh fixture; independently writable
subtests also create separate fixtures. Read-only subtests may share their
parent's fixture. Helpers close the pool before dropping the fixture; a pool
close failure skips the drop and reports the retained fixture path. Test
statements/rows/transactions are released before either. A setup failure is a
test failure, not a skip. The helper pings before returning the pool so an
attachment failure is identified as setup rather than a contract assertion.

Cleanup uses a fresh timeout and `isql` to drop only the owned database, then
removes its empty directory. Failures before a file is created remove the empty
directory without attempting a spurious drop. Failed drops retain artifacts
and report their location; the harness never recursively deletes a possibly
attached database. Hard process termination can prevent cleanup, leaving an
owned temporary directory to inspect after attachments have stopped.

Subprocess runtime, pipe wait, and captured output are bounded. Error output
redacts raw and SQL-escaped credentials; excessive output is omitted entirely.
Owned subprocess groups are terminated on execution failure. Native driver
calls remain non-interruptible in flight, so the Go process timeout is still
necessary. No filesystem permissions or server security settings are changed.

The tested InterBase isql returns exit status 1 even after successful SQL.
The harness therefore requires a standalone SQL completion marker, complete
captured output, no SQL error diagnostics, and a zero/one exit status. It also
verifies that creation produced the owned file and that DROP removed it. Exit
status 1 alone is never evidence of successful provisioning or cleanup.

## Docker-Backed Runs

The sibling `interbase-server-docker` project supplies the server image. Use a
dedicated container instead of its normal Compose deployment, which mounts
persistent databases, backups and logs. Set `IMAGE` to an existing local image;
these commands do not build, pull or publish a licensed image:

### Automated runner

The repository runner owns the container and a unique private temporary root.
It binds only `127.0.0.1:3050`, mounts that same absolute host path into the
container, uses tmpfs for `/var/lib/interbase`, and disables automatic backups.
It copies the image's matching `libgds.so` and `isql` into that temporary root;
the host `/opt/interbase` installation is never modified. The runner first runs
`TestReadFixtureSmoke` with isolated `SYSDBA`/`masterkey` credentials. It
retries that startup check for at most five attempts or 30 seconds, with a
one-second delay, and prints only the final error after sanitizing it. It then
requires the same smoke check to fail with an invalid password before running
the requested Go arguments once. Invalid-password readiness accepts only the
specific InterBase authentication diagnostic (SQLSTATE 28000, status
335544472, or the standard `Your user name and password are not defined`
message); generic connection failures and SQLCODE -902 alone are rejected.

```sh
# Use an image that is already present in the local Docker daemon.
IMAGE='sha256:2787b636c0c39d3eeab23d9365d30292e45015704cbd9cdee42510d21a043f73' \
  make test-integration-docker

# Run a focused contract selection; the runner still performs readiness checks.
IMAGE='sha256:2787b636c0c39d3eeab23d9365d30292e45015704cbd9cdee42510d21a043f73' \
  ./scripts/test-integration-docker.sh -run '^TestReadFixtureSmoke$'
```

With no extra Go arguments, the requested run defaults to `-count=1` and
`-timeout=10m`. Pass `GO_TEST_ARGS='-run ^TestRead'` to the Make target or pass
arguments directly to the script; all arguments are forwarded to `go test`.
`make test-runner` runs the orchestration tests with Bats and controlled
`docker`/`go` executable doubles. A pinned local Bats checkout can be used
without a global install with `make BATS=/path/to/bats/bin/bats test-runner`.

Operational `INTERBASE_DATABASE`, `INTERBASE_USER`, and
`INTERBASE_PASSWORD`, plus test fixture overrides, are removed before the Go
smoke and contract commands. The runner prints no Docker logs or credential
values. On failure or interruption it stops and removes only the container ID
it started, removes only its copied files, and uses nonrecursive directory
cleanup. If fixture artifacts remain, their directory is reported and retained
for inspection rather than recursively deleted.

```sh
TEST_ROOT=$(mktemp -d /tmp/interbase-go-docker.XXXXXX)
TEST_CONTAINER="interbase-go-test-${TEST_ROOT##*.}"
docker run --pull=never --detach --rm --name "$TEST_CONTAINER" \
  --publish 127.0.0.1:3050:3050 \
  --mount "type=bind,source=$TEST_ROOT,target=$TEST_ROOT" \
  --mount type=tmpfs,destination=/var/lib/interbase \
  --env IB_SYSDBA_USER=SYSDBA --env IB_SYSDBA_PASSWORD=masterkey \
  --env IB_DATA_DIR=/var/lib/interbase/data \
  --env IB_BACKUP_DIR=/var/lib/interbase/backups --env IB_BACKUP_DATABASES= \
  "${IMAGE:?set IMAGE to the local InterBase server image}"
```

The same-path bind mount is required: the host creates the temporary directories
and the server creates the database files at those exact absolute paths. The
image used for validation runs as root under rootful Docker; other UID mappings
need their own permission validation. Do not broaden host permissions globally.
Wait for the server to accept the configured password and reject a wrong one
before running tests. Do not bind the test server to all host interfaces.

In the validated setup, the installed 14.7 client library failed authentication
against the container's 15.1 server. A temporary copy of the image's client
library resolved it; no host installation or driver source was changed:

```sh
docker cp -L "$TEST_CONTAINER:/opt/interbase/lib/libgds.so" "$TEST_ROOT/libgds.so"
docker cp -L "$TEST_CONTAINER:/opt/interbase/bin/isql" "$TEST_ROOT/isql"

env -u INTERBASE_DATABASE -u INTERBASE_USER -u INTERBASE_PASSWORD \
  LD_LIBRARY_PATH="$TEST_ROOT:/opt/interbase/lib" TMPDIR="$TEST_ROOT" \
  INTERBASE_TEST_ISQL="$TEST_ROOT/isql" \
  INTERBASE_TEST_USER=SYSDBA INTERBASE_TEST_PASSWORD=masterkey \
  go test -tags=integration ./integration -count=1 -timeout=10m
```

Run the same command with `-run '^TestReadFixtureSmoke$'` first to check setup.
After testing, **even when the test command fails**, stop only this container:

```sh
docker stop "$TEST_CONTAINER"
rm -- "$TEST_ROOT/isql" "$TEST_ROOT/libgds.so"
rmdir -- "$TEST_ROOT"
```

The nonrecursive `rmdir` intentionally fails if a fixture was retained. Inspect
that path rather than recursively deleting it. The container's tmpfs security
database disappears with the container; existing Docker volumes are untouched.
Never commit the temporary vendor binaries or license files.

## Coverage Map

Source methods below are selected/adapted assertions, not whole-method parity.
Each contract file also carries source-method comments. Go-specific lifecycle
checks (contexts, pool reuse, `sql.ErrTxDone`) supplement the Python behavior.

| Go groups | Python source | Adaptation |
| --- | --- | --- |
| `TestReadConnectionLifecycle`, `TestReadRepeatedEarlyClose` | `TestCursor.test_exec_after_close`, `.test_iteration`; `DatabaseAPI20Test.test_connect`, `.test_close`; `TestBugs.test_pyib_34` | Pool release/reuse and explicit row cleanup, not reusable Python cursor objects |
| `TestReadRowsColumnsAndEOF`, `TestReadEmptyRowsAndErrNoRows` | `TestCursor.test_iteration`, `.test_description`; `DatabaseAPI20Test.test_fetchone`, `.test_fetchmany`, `.test_fetchall` | Ordered rows, column names, `Rows.Err`, EOF and `sql.ErrNoRows`; three-country fixture instead of the original 14 |
| `TestReadNullAndEmptyString`, `TestTypesNullAndEmptyText` | `DatabaseAPI20Test.test_None`; `TestCharsetConversion.testCharVarchar` | `sql.NullString`, preserving NULL versus empty |
| `TestReadPositionalCasts`, `TestReadPositionalFilters` | `DatabaseAPI20Test.test_execute`; `TestInsertData.test_insert_integers`, `.test_insert_float_double`, `.test_insert_boolean` | `?` binding through SELECT, including special characters, UTF8 and NULL |
| `TestReadRecoveryAfterInvalidQuery`, `TestReadRecoveryAfterArgumentCountError`, `TestReadPreCancelledQuery` | `DatabaseAPI20Test.test_execute`, `.test_fetchone`; `TestBugs.test_pyib_35` | Go query-error recovery and added cancellation checks, not exact Python exception strings |
| `TestReadCHARPadding`, `TestReadIntegerBoundaries`, `TestReadTimestampFraction` | `TestInsertData.test_insert_char_varchar`, `.test_insert_integers`, `.test_insert_datetime` | Preseeded decoding independent of driver writes |
| `TestReadNumeric9ExactStrings`, `TestReadWideDecimalDialectOneFloatTolerance` | `TestInsertData.test_insert_numeric_decimal` | Exact scaled-integer strings versus Dialect-1 approximate wide numerics |
| `TestReadSelectableBudgetProcedure` | `TestStoredProc.test_callproc` | SQL SELECT from a selectable procedure, four original aggregate values; no `callproc` API or integer-to-CHAR extension assertion |
| `TestCharsetUTF8InsertReadAcrossAttachments` | `TestCharsetConversion.test_utf82win1250` | Original five-field T4 row inserted and committed through UTF8, then read through WIN1250 and UTF8 attachments to the same database; exact original values, column lengths and charsets |
| `TestReadWIN1250UTF8RoundTrip` | Go-specific regression inspired by `TestCharsetConversion.test_utf82win1250` | Explicit UTF8 input cast followed by WIN1250 and UTF8 conversions on a UTF8 attachment; this SQL does not appear in the upstream test and is not a substitute for its persisted-row contract |
| `TestReadVarcharParameterLengths`, `TestReadVarchar5000Cast` | `TestBugs.test_pyib_22`, `.test_pyib_25` | Read-side parameter regressions; does not claim the original insert loop |
| `TestWriteDMLRowsAffected`, `TestWriteDMLRoundTrip`, `TestWriteDuplicateKeyRecovery` | `DatabaseAPI20Test.test_rowcount`, `.test_execute`, `.test_executemany`; `TestConnection.test_connection` | Exec, exact counts and persisted rows, zero-row operations and constraint recovery; no batch API |
| `TestWritePreparedSelectReuseAndArgumentCounts`, `TestWritePreparedExecRepeated`, `TestWritePreparedCloseRejectsUse` | `TestPreparedStatement.test_execution`; `DatabaseAPI20Test.test_execute`, `.test_executemany` | `sql.Stmt` reuse and closure, not Python cursor ownership |
| `TestWriteNumericParameterConversions` | `TestInsertData.test_insert_integers`, `.test_insert_float_double` | Adds standard Go numeric input conversion requirements |
| `TestTransactionCommitVisibilityAndOwnWrites`, `TestTransactionRollbackVisibilityAndOwnWrites` | `TestTransaction.test_cursor`, `.test_context_manager`; `DatabaseAPI20Test.test_cursor_isolation` | Own-write visibility and commit/rollback checked through a separate pinned attachment |
| `TestTransactionSavepointPartialRollback`, `TestTransactionReadOnlyOption` | `TestTransaction.test_savepoint`, `.test_tpb` | SQL savepoint and Go read-only transaction option; no binary TPB API |
| `TestTransactionPrepareContextCommit`, `TestTransactionStmtContextRollback`, `TestTransactionErrTxDoneAfterCommitAndRollback` | `TestPreparedStatement.test_execution`; `TestTransaction.test_cursor`; `DatabaseAPI20Test.test_commit`, `.test_rollback` | Additional Go statement/transaction binding and completion requirements |
| `TestTypesIntegerBounds`, `TestTypesCharVarcharUTF8RoundTrip`, `TestTypesUTF8OverlengthRecovery` | `TestInsertData.test_insert_integers`, `.test_insert_char_varchar`; `TestCharsetConversion.testCharVarchar` | Insert/readback, padding, UTF8 character capacity and recovery without truncation |
| `TestTypesFloatTolerance`, `TestTypesNumericDecimalBindings` | `TestInsertData.test_insert_float_double`, `.test_insert_numeric_decimal` | NaN-safe tolerances, decimal strings/float inputs; no Python Decimal library |
| `TestTypesTimestampPrecisionAndMidnight`, `TestTypesBooleanRoundTrip` | `TestInsertData.test_insert_datetime`, `.test_insert_boolean`; `TestBugs.test_pyib_44` | `time.Time` fractional/midnight values and boolean readback; no distinct Python date object |
| `TestTypesTextAndBinaryBlobRoundTrips`, `TestTypesEmptyBytesDistinguishNull` | `TestInsertData.test_insert_blob`; `TestCharsetConversion.testBlob`; `TestBugs.test_pyib_30`; `DatabaseAPI20Test.test_Binary`, `.test_None` | Text/binary BLOB content, UTF8, embedded NUL, 90,000-byte segments and empty versus NULL; no stream reader, OCTETS column or subtype-2 coverage |
| `TestTypesVarchar5000Insert` | `TestBugs.test_pyib_25` | Actual DDL/insert/readback for lengths 0, 1 and 5000 |

## Deferred Coverage

- Dialect-3-only DATE/TIME and high-precision exact decimal semantics await a
  public dialect selection API. Wide Dialect-1 numerics use float tolerances.
- Python module attributes, exception hierarchy, cursor fetch/map APIs,
  description tuples, `arraysize`, and input/output sizing do not map directly
  to `database/sql`.
- Private DPB/SQLDA fields, exact execution plans, version-specific catalog
  counts, role configuration, transaction-info bytes, retaining transactions,
  and explicit TPB objects are not part of this migration.
- Arrays, stream-BLOB seek/read APIs, events, change views, distributed
  transactions, schema object models, services, backup/restore and encryption
  administration remain separate extension suites.
- This is not exhaustive bug-suite parity. Trigger-default regression
  `test_pyib_17`, stream and alternate BLOB subtype behavior in `test_pyib_30`,
  and the original writable `test_pyib_22` sweep are not fully migrated.

## Interpreting Results

The original read-only baseline intentionally rejected Exec, public Prepare, BeginTx,
`time.Time`/`[]byte` arguments, ordinary Go numeric argument conversions, BLOB
results and scaled floating-point results. The implementation now supports
those capabilities. There are no feature skips or expected-failure wrappers;
any failing contract makes the live test command fail.

### Current implementation

On 2026-09-13 the full suite had **54 passing top-level groups, zero failures
and zero skips**, against server `LI-V15.1.0.49` and client `LI-V15.1.0.42`.
All 255 VARCHAR-length subtests passed. New regressions cover actual native
statement reuse, repeated cursors within an explicit transaction, SQL
transaction-control rejection, and text BLOB conversion across attachments.

`TestCharsetUTF8InsertReadAcrossAttachments` passed through both WIN1250 and
UTF8 attachments without a driver change. It faithfully reproduces the upstream
`test_utf82win1250` insert, explicit commit, and full five-field row comparison.
Only unused T4 columns are omitted from its isolated schema. This establishes
the upstream persisted-row behavior independently of the nested-cast test.

`TestReadWIN1250UTF8RoundTrip` now declares its input parameter as UTF8 before
the WIN1250 and UTF8 conversions. The attachment, input text, exact output
assertion, and EOF checks are retained; no driver SQL rewriting, feature skips,
or expected-failure wrappers were added. This corrects the synthetic query's
parameter typing, not support for the original shorthand on the native stack.

The automated Docker runner now passes the full suite and removes its
container and private temporary root. Before the SQL correction it reproduced
the failure, returned nonzero, and completed the same cleanup. The runner's
controlled Bats suite has 15 tests, including readiness retries, failure
propagation and interruption cleanup.

### Mixed-character-set parameters

A standalone C probe using matching InterBase 15.1 headers and client, a
Dialect-1 UTF8 attachment, and the same 30-character Czech input isolated the
native behavior independently of the Go driver. Input representations were
30 WIN1250 bytes and 60 UTF8 bytes. All three cases described a UTF8 output
capacity of 160 bytes, so an undersized output buffer was not the cause.

| Native query/binding | Input descriptor | Result |
| --- | --- | --- |
| Original nested cast, described WIN1250 bytes | VARCHAR, charset 51, 40 bytes | Execute succeeds; fetch fails with SQLCODE -802, primary status 335544321 and nested `isc_transliteration_failed` (335544565) |
| Original nested cast, descriptor overridden to UTF8 | VARCHAR, charset 59, 160 bytes | Execute fails with SQLCODE -303 |
| Explicit UTF8 input cast, described UTF8 bytes | VARCHAR, charset 59, 160 bytes | Execute and fetch succeed; all 60 returned bytes match the expected UTF8 text |

The supported form tested by the Go regression is:

```sql
SELECT CAST(
    CAST(
        CAST(? AS VARCHAR(40) CHARACTER SET UTF8)
        AS VARCHAR(40) CHARACTER SET WIN1250
    ) AS VARCHAR(40) CHARACTER SET UTF8
) FROM RDB$DATABASE
```

The original shorthand omitted the innermost UTF8 cast. Its native failure
remains a limitation of the tested query/binding combination; these results
do not establish a general server defect or behavior on other versions.

### Migration baseline

The first host-only attempt was blocked by a missing local server. On
2026-09-12, Docker-backed validation used server `LI-V15.1.0.49` and client
`LI-V15.1.0.42` with the existing driver compiled against the installed SDK.
Provisioning and cleanup passed, including the complete fixture schema.

The full suite had **19 passing and 28 failing top-level groups**: the smoke
test, two helper checks, and 16 read contracts passed. All 255 individually
named VARCHAR-length subtests passed. Three read contracts failed:

- `TestReadCHARPadding`: UTF8 `CHAR(5)` decoded as 20 bytes of padded text
  (`"AA"` plus 18 spaces), instead of five characters.
- `TestReadWideDecimalDialectOneFloatTolerance`: the driver explicitly rejects
  scaled floating-point results.
- `TestReadWIN1250UTF8RoundTrip`: the bound query failed with SQLCODE -303. The
  same charset conversion with a UTF8 SQL literal succeeded through isql,
  narrowing this to parameter handling rather than unavailable server support.

The other 25 failing groups exercise intentionally unfinished writes,
transactions, public preparation and additional argument/BLOB types. They now
fail on actual driver capabilities, not fixture setup. No driver behavior was
changed or assertions relaxed to obtain these results. This validates the
tested image/client combination, not general 14.x/15.x compatibility or a
passing implementation of the full contract suite.

Attribution and permission notices are retained in `UPSTREAM_LICENSE.txt`.
