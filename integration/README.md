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
| `TestReadWIN1250UTF8RoundTrip` | `TestCharsetConversion.test_utf82win1250` | Nested charset casts on a UTF8 attachment, not a configurable WIN1250 connection |
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

The baseline driver intentionally rejects Exec, public Prepare, BeginTx,
`time.Time`/`[]byte` arguments, ordinary Go numeric argument conversions, BLOB
results and scaled floating-point results. Those contracts are expected to
remain red until implemented; there are no feature skips or expected-failure
wrappers. A driver defect can also make a read-side contract fail.

Offline tests, tagged compilation and static reviews do not prove live SQL
compatibility. During migration on this machine, the local smoke test reached
isql but failed to connect to `localhost/3050`: only the client installation was
available. Consequently no successful live contract run or healthy create/drop
cycle has yet been established. Setup failures must not be reported as the
expected unsupported-driver failures. The failed setup left no fixture files.

Attribution and permission notices are retained in `UPSTREAM_LICENSE.txt`.
