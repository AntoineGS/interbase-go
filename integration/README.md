# Python-Derived Database Contracts

This is a test-first migration of selected behavior from
[Embarcadero/InterBasePython](https://github.com/Embarcadero/InterBasePython),
source commit `f2dcefccd1a3fcf0f06d72849a615aba4214106c`.
It targets the Go `database/sql` API plus a bounded direct cursor/transaction
surface for Python-parity behavior that `database/sql` cannot represent.

The suite includes a live fixture smoke test, offline helper tests, and
table-driven contracts, including every VARCHAR parameter length from 0 through
254. Source mappings identify adapted assertions rather than whole Python
methods ported unchanged.

## Run

From the repository/worktree root:

```sh
# Compile all contracts without provisioning a database.
go test -tags=integration ./integration -run '^$'

# Run integration-helper regressions without a database server.
go test -tags=integration ./integration -run '^(TestPingDatabase|TestCleanupDatabase)' -count=1

# First check local provisioning, attachment, querying, and cleanup.
go test -tags=integration ./integration -run '^TestReadFixtureSmoke$' -v -count=1 -timeout=2m

# Read-only schema catalog and DDL contracts.
go test -tags=integration ./integration -run '^TestSchema' -v -count=1 -timeout=5m

# Read-side contracts, including regressions that may expose driver bugs.
go test -tags=integration ./integration -run '^TestRead' -v -count=1 -timeout=5m

# Full contract suite.
go test -tags=integration ./integration -v -count=1 -timeout=10m

# Direct attachment, cursor, retaining, plan/info, indicator, and Services
# Manager contracts.
go test -tags=integration ./integration -run '^TestDirect' -v -count=1 -timeout=5m

# Native owned-database lifecycle, duplicate preservation, diagnostics, and
# no-execution DML planning contracts.
go test -tags=integration ./integration -run '^TestDirectLifecycle' -v -count=1 -timeout=5m

# Services Manager contracts only.
go test -tags=integration ./integration -run '^TestServices' -v -count=1 -timeout=5m

# Native lifecycle race regression: persistent workers plus physical attachment
# churn. The test is opt-in because a client-library abort terminates the process.
INTERBASE_NATIVE_LIFECYCLE_RACE=1 go test -tags=integration ./integration \
  -run '^TestNativeLifecycleRace$' -v -count=1 -timeout=2m

# Required four-worker lifecycle soak.
INTERBASE_SOAK=1 INTERBASE_SOAK_DURATION=120s INTERBASE_SOAK_WORKERS=4 \
  go test -tags=integration ./integration -run '^TestSoakConcurrentWorkload$' \
  -v -count=1 -timeout=3m

# Client-death distributed limbo recovery through the real Services Manager.
go test -tags=integration ./integration \
  -run '^TestDistributedRecoveryThroughServicesAfterClientDeath$' \
  -v -count=1 -timeout=10m
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
- Fixture dialect defaults to 3 and can be selected as 1 or 3 by the test
  configuration. Existing Dialect 1 contracts opt in explicitly. Fixture
  create/drop scripts and `isql` use that same selected dialect; dialect
  selection is not an environment configuration option.

Optional overrides are `INTERBASE_TEST_ISQL`, `INTERBASE_TEST_SERVER`,
`INTERBASE_TEST_USER`, and `INTERBASE_TEST_PASSWORD`. `INTERBASE_TEST_SERVER`
is a native server prefix such as `localhost/3050`; it is useful when the
matching client is copied from a Docker image onto a host that has no default
`gds_db` service entry. An unset password selects `masterkey`; an explicitly
empty password remains empty. Credential defaults are confined to this test
helper, not the driver or CLI. Use your normal secure environment tooling for
custom credentials; no credential files are loaded automatically.

The temporary root is the platform temporary directory (`TMPDIR` when set).
It must be an absolute local path without colons or control characters. The
harness creates a private, unique `interbase-go-fixture-*` directory and a new
`database.ib` inside it. The direct lifecycle tests additionally create private
`interbase-go-direct-lifecycle-*` directories and a quoted database filename.
They accept **no existing database target or remote
attachment string**, and ignore the application's `INTERBASE_DATABASE`,
`INTERBASE_USER`, and `INTERBASE_PASSWORD` settings.

## Fixture Lifecycle

`internal/testfixture` runs InterBase `isql` directly, with SQL on standard
input and no shell or credential command-line arguments. It creates the small
schema in `testdata/schema.sql` and selects the configured dialect for both
creation and cleanup. When `INTERBASE_TEST_SERVER` is set, the generated local
path is passed to `isql` as a server-prefixed attachment (for example,
`localhost/3050:/tmp/...`), while the ownership checks still inspect only the
local generated path. It does not run the original fixture's security-user or
encryption administration statements.

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

The direct lifecycle tests use `Attachment.DropDatabase` for their generated
databases. The first attachment is detached before the duplicate-create check;
the sentinel row is then verified through a separate direct attachment before
that owned path is dropped. A live `DropDatabase` refusal with an active
transaction or cursor is retried only after those resources are released.

`TestDistributedRecoveryThroughServicesAfterClientDeath` creates two separate
owned databases, deliberately advances only the first database's transaction
counter, and starts the distributed group in a child process. The child writes
the copied recovery list to a private JSON file, syncs and closes it, prepares
the group, and exits without deferred rollback. The serialized JSON contains
only the two database paths and copied `RecoveryInfo` entries; it does not
contain credentials or SQL values. The parent matches each transaction ID
through `ListLimbo`; only after every exact match does the parent write the
explicit `commit\n` or `rollback\n` decision, sync and close that private file,
then resolve both participants with `CommitLimbo` or `RollbackLimbo` and verify
both sentinel rows. The test exercises client-process death only: it does not
test a server restart or power loss, and it does not claim power-loss
durability (which would additionally require an application-specific atomic
rename and directory sync protocol). The Docker runner supplies the matching
`/opt/interbase/bin/isql` inside the owned container; a host `isql` installation
is not required.

Subprocess runtime, pipe wait, and captured output are bounded. Error output
redacts raw and SQL-escaped credentials; excessive output is omitted entirely.
Owned subprocess groups are terminated on execution failure. Non-DSQL native
driver calls remain non-interruptible in flight, and DSQL cancellation is best
effort rather than a deadline guarantee, so the Go process timeout is still
necessary. No filesystem permissions or server security settings are changed.

The tested InterBase isql returns exit status 1 even after successful SQL.
The harness therefore requires a standalone SQL completion marker, complete
captured output, no SQL error diagnostics, and a zero/one exit status. It also
verifies that creation produced the owned file and that DROP removed it. Exit
status 1 alone is never evidence of successful provisioning or cleanup.

## Docker-Backed Runs

### Isolated native TLS security contracts

> **Security warning:** with vendor client `LI-V15.1.0.42`, trusting the test
> CA does not prove that the native client verified the server identity. The
> strict matrix intentionally fails its wrong-hostname cases; it is not part of
> the ordinary full Docker integration pass.

```sh
IMAGE=interbase-go-parity-test:local \
  INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
  bash scripts/test-tls-docker.sh
```

This separate runner tests root `database/sql`, root direct attachments,
`events.Subscribe`, and `services.Open` independently; the packages currently
have separate native attachment-target builders. It uses a **new disposable
`--network none` container**, with no published ports or host bind mounts. The
image must already exist locally and support the sibling server project's
entrypoint; it is never built, pulled, pushed, or modified. Only Linux/amd64 has
been exercised.

OpenSSL generates a private two-day CA, an unrelated CA, and a server certificate
with CN/SAN `localhost` and serverAuth EKU. Private keys and the random temporary
database password remain in mode-0700 temporary storage/mode-0600 files and are
removed on exit. The container's data directory is tmpfs. The runner writes only
the documented `IBSSL_SERVER_HOST_NAME`, `IBSSL_SERVER_PORT_NO`, and
`IBSSL_SERVER_CERTFILE` parameters in its disposable
`/opt/interbase/secure/server/ibss_config`. It does not alter host InterBase
configuration, reduce TLS security levels, or install weak DH parameters.

The tests require these real native outcomes for each API:

| Scenario | Required result |
| --- | --- |
| Correct CA and hostname | Successful attachment plus SQL query, diagnostics, event baseline, or service-version query |
| Unrelated CA | Native rejection, bracketed by successful TLS operations |
| Wrong DNS hostname at the same reachable listener | Native rejection |
| TLS required against a working plaintext listener | Native rejection, never plaintext fallback |

The plaintext control exists only inside the disconnected disposable container.
The fixture maps `wrong.parity.invalid` to `::1`, checks that both names reach the
same endpoint, validates the certificate chain offline, and proves the leaf
certificate does not identify the wrong name. Authentication readiness requires
both a successful login and rejection of a wrong password. Non-DSQL native
calls cannot be canceled after entry, and DSQL cancellation is best effort, so
both the binary and Docker execution have timeouts. Container ownership is
checked before cleanup; cleanup failures return failure and identify any
retained private artifacts.

**Known security failure (2026-09-16):** vendor client `LI-V15.1.0.42` accepts
the wrong hostname, reproduced with vendor `isql` and with all four Go APIs
(`database/sql`, direct attachments, events, and Services). The strict matrix
has **12 passing and 4 failing** cases: correct trust, unrelated-CA rejection,
and required-TLS/no-plaintext-fallback pass; the four wrong-hostname rejections
fail. The runner intentionally returns failure rather than skipping or accepting
this defect. Do not treat `TLS.Enabled` plus CA trust as proof of server-name
verification for this client. No native-core or Go-side TLS preflight workaround
is implemented: an independent connection would not authenticate the actual
InterBase connection. Vendor clarification or a corrected native client is
required before this matrix can pass.

Without `INTERBASE_TLS_TEST=1`, `TestNativeTLSVerification` skips explicitly, so
ordinary integration fixtures are unaffected. The runner supplies
`INTERBASE_TLS_DATABASE`, `INTERBASE_TLS_USER`, `INTERBASE_TLS_PASSWORD`,
`INTERBASE_TLS_CA`, `INTERBASE_TLS_WRONG_CA`, and `INTERBASE_TLS_SERVER_CERT`;
paths refer to the container, not a host application. Manual execution must use
the same isolated fixture. These contracts do not cover mTLS/client passphrases,
`ServerPublicPath`, event-payload encryption, or protocol/cipher policy.

The sibling `interbase-server-docker` project supplies the server image. Use a
dedicated container instead of its normal Compose deployment, which mounts
persistent databases, backups and logs. Set `IMAGE` to an existing local image;
these commands do not build, pull or publish a licensed image:

### Automated runner

The repository runner owns the container and a unique private temporary root.
It binds only `127.0.0.1:3050`, mounts that same absolute host path into the
container, uses tmpfs for `/var/lib/interbase`, and disables automatic backups.
It copies the image's matching `libgds.so` and `isql` into that temporary root.
It compiles the tagged integration and services test binaries on the host using
the matching official headers from `INTERBASE_INCLUDE`, then executes those
binaries inside the container; the host `/opt/interbase` installation is never
modified. The runner first runs
`TestReadFixtureSmoke` with isolated `SYSDBA`/`masterkey` credentials. It
retries that startup check for at most five attempts or 30 seconds, with a
one-second delay, and prints only the final error after sanitizing it. It then
requires the same smoke check to fail with an invalid password before running
the requested Go arguments once. Invalid-password readiness accepts only the
specific InterBase authentication diagnostic (SQLSTATE 28000, status
335544472, or the standard `Your user name and password are not defined`
message); generic connection failures and SQLCODE -902 alone are rejected.
Before the requested integration package, the runner also executes the native
Services framing contract in `./services`. That check starts a real log service,
inspects the raw `isc_info_svc_to_eof` response for its length-delimited payload
and terminal marker, and then validates the same frame with the Go decoder.

```sh
# Use an image that is already present in the local Docker daemon.
INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
IMAGE='sha256:2787b636c0c39d3eeab23d9365d30292e45015704cbd9cdee42510d21a043f73' \
  make test-integration-docker

# Run a focused contract selection; the runner still performs readiness checks.
INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
IMAGE='sha256:2787b636c0c39d3eeab23d9365d30292e45015704cbd9cdee42510d21a043f73' \
  ./scripts/test-integration-docker.sh -run '^TestReadFixtureSmoke$'
```

With no extra Go arguments, the requested run defaults to `-count=1` and
`-timeout=10m`. Pass `GO_TEST_ARGS='-run ^TestRead'` to the Make target or pass
arguments directly to the script; supported Go test flags are translated to the
compiled test binary.
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

INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
CGO_CFLAGS='-I/tmp/opencode/interbase-parity-include' \
CGO_LDFLAGS="-L$TEST_ROOT -Wl,-rpath,$TEST_ROOT" \
go test -tags=integration -c ./integration -o "$TEST_ROOT/integration.test"
docker cp "$TEST_ROOT/integration.test" "$TEST_CONTAINER:$TEST_ROOT/integration.test"
LD_LIBRARY_PATH="$TEST_ROOT:/opt/interbase/lib" TMPDIR="$TEST_ROOT" \
INTERBASE_TEST_ISQL="$TEST_ROOT/isql" INTERBASE_TEST_SERVER=localhost/3050 \
INTERBASE_TEST_USER=SYSDBA INTERBASE_TEST_PASSWORD=masterkey \
docker exec --env LD_LIBRARY_PATH --env TMPDIR \
  --env INTERBASE_TEST_ISQL --env INTERBASE_TEST_SERVER \
  --env INTERBASE_TEST_USER --env INTERBASE_TEST_PASSWORD \
  "$TEST_CONTAINER" "$TEST_ROOT/integration.test" \
  -test.count=1 -test.timeout=10m
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
The completed additions are in `parity_test.go`, `procedure_test.go`,
`metadata_test.go`, `dialect_test.go`, and `services_test.go`; their rows below
retain their source mapping. Go-specific lifecycle checks (contexts, pool reuse,
`sql.ErrTxDone`) supplement the Python behavior.

| Go groups | Python source | Adaptation |
| --- | --- | --- |
| `TestReadConnectionLifecycle`, `TestReadRepeatedEarlyClose` | `TestCursor.test_exec_after_close`, `.test_iteration`; `DatabaseAPI20Test.test_connect`, `.test_close`; `TestBugs.test_pyib_34` | Pool release/reuse and explicit row cleanup, not reusable Python cursor objects |
| `TestReadRowsColumnsAndEOF`, `TestReadEmptyRowsAndErrNoRows` | `TestCursor.test_iteration`, `.test_description`; `DatabaseAPI20Test.test_fetchone`, `.test_fetchmany`, `.test_fetchall` | Ordered rows, column names, `Rows.Err`, EOF and `sql.ErrNoRows`; three-country fixture instead of the original 14 |
| `TestReadNullAndEmptyString`, `TestTypesNullAndEmptyText` | `DatabaseAPI20Test.test_None`; `TestCharsetConversion.testCharVarchar` | `sql.NullString`, preserving NULL versus empty |
| `TestReadPositionalCasts`, `TestReadPositionalFilters` | `DatabaseAPI20Test.test_execute`; `TestInsertData.test_insert_integers`, `.test_insert_float_double`, `.test_insert_boolean` | `?` binding through SELECT, including special characters, UTF8 and NULL |
| `TestReadRecoveryAfterInvalidQuery`, `TestReadRecoveryAfterArgumentCountError`, `TestReadPreCancelledQuery` | `DatabaseAPI20Test.test_execute`, `.test_fetchone`; `TestBugs.test_pyib_35` | Go query-error recovery and added cancellation checks, not exact Python exception strings |
| `TestReadCHARPadding`, `TestReadIntegerBoundaries`, `TestReadTimestampFraction` | `TestInsertData.test_insert_char_varchar`, `.test_insert_integers`, `.test_insert_datetime` | Preseeded decoding independent of driver writes |
| `TestReadNumeric9ExactStrings`, `TestReadWideDecimalDialectOneFloatTolerance` | `TestInsertData.test_insert_numeric_decimal` | Exact scaled-integer strings versus Dialect-1 approximate wide numerics |
| `procedure_test.go`: `TestProcedureDirectQueryReturnsOneOutputRowAndEOF`, `TestProcedurePreparedQueryReusesHandleForStringAndIntegerInputs` | `TestStoredProc.test_callproc` | Executable procedure output with both CHAR and integer inputs, one output row, EOF, and prepared reuse |
| `procedure_test.go`: `TestProcedureExecRunsZeroOutputProcedureAndPersistsSideEffect`, `TestProcedureExecRejectsOutputWithoutExecutingIt`, `TestProcedureQuery*` | `TestStoredProc.test_callproc` | Executable-procedure write, output-discard protection, implicit completion/rollback behavior, recovery, and caller-owned explicit transactions |
| `metadata_test.go`: `TestMetadata*` | `TestCursor.test_description`; `DatabaseAPI20Test.test_description` | Go `ColumnTypes` adaptation: names, types, scan types, lengths, nullable state, and precision/scale; aliases and attributable columns are described conservatively, while expressions and uncertain properties remain unknown |
| `char_root_test.go`: `TestReadRootCHARExpressions*`, `TestReadPreparedRootCHARExpression*` | Go-specific regression | Source-less fixed-width `CHAR(n)` value padding, per-column attribution, prepared results, and complete-value preservation for concatenation, nested SELECTs, derived stars, comments, quoted aliases, and set operations |
| `dialect_test.go`: `TestDialect*` | `TestInsertData.test_insert_datetime`, `.test_insert_numeric_decimal` | Dialect 3 attachment/fixture selection, DATE/TIME/TIMESTAMP, exact scaled NUMERIC/DECIMAL strings, and Dialect-1 server-conversion preservation; Go returns strings for scaled Dialect-3 values rather than Python `Decimal` objects |
| `parity_test.go`: `TestParityTriggerDefault`, `TestParityVarcharInsertLengths`, `TestParityOctets` | `TestBugs.test_pyib_17`, `.test_pyib_22`; `TestCharsetConversion.test_octets` | Trigger/default, writable VARCHAR lengths, and raw OCTETS CHAR/VARCHAR bytes, including fixed padding, NULL/empty, and direct/prepared paths |
| `TestCharsetUTF8InsertReadAcrossAttachments` | `TestCharsetConversion.test_utf82win1250` | Original five-field T4 row inserted and committed through UTF8, then read through WIN1250 and UTF8 attachments to the same database; exact original values, column lengths and charsets |
| `TestEventsAccumulateNotifications`, `TestEventsCanceledNextPreservesNotification`, `TestEventsRequireCommitAndIgnoreRollback` | InterBase event callback lifecycle | Explicitly owned event attachment, counts accumulated across callbacks, cancellation preserving pending notifications, multi-attachment fixture coverage, and strict committed-write/rollback timing checks |
| `TestReadWIN1250UTF8RoundTrip` | Go-specific regression inspired by `TestCharsetConversion.test_utf82win1250` | Explicit UTF8 input cast followed by WIN1250 and UTF8 conversions on a UTF8 attachment; this SQL does not appear in the upstream test and is not a substitute for its persisted-row contract |
| `TestReadVarcharParameterLengths`, `TestReadVarchar5000Cast` | `TestBugs.test_pyib_22`, `.test_pyib_25` | Read-side parameter regressions; does not claim the original insert loop |
| `TestWriteDMLRowsAffected`, `TestWriteDMLRoundTrip`, `TestWriteDuplicateKeyRecovery` | `DatabaseAPI20Test.test_rowcount`, `.test_execute`, `.test_executemany`; `TestConnection.test_connection` | Exec, exact counts and persisted rows, zero-row operations and constraint recovery; no batch API |
| `TestWritePreparedSelectReuseAndArgumentCounts`, `TestWritePreparedExecRepeated`, `TestWritePreparedCloseRejectsUse` | `TestPreparedStatement.test_execution`; `DatabaseAPI20Test.test_execute`, `.test_executemany` | `sql.Stmt` reuse and closure, not Python cursor ownership |
| `cursor_parity_test.go`: `TestConnectionAllowsMultipleImplicitCursorsAndExecWhileRowsAreActive`, `TestExplicitTransactionCompletionDoesNotCloseImplicitCursors` | Go parity regression | Multiple native cursors, writes beside open rows, and selective explicit-transaction cursor cleanup |
| `TestWriteNumericParameterConversions` | `TestInsertData.test_insert_integers`, `.test_insert_float_double` | Adds standard Go numeric input conversion requirements |
| `TestTransactionCommitVisibilityAndOwnWrites`, `TestTransactionRollbackVisibilityAndOwnWrites` | `TestTransaction.test_cursor`, `.test_context_manager`; `DatabaseAPI20Test.test_cursor_isolation` | Own-write visibility and commit/rollback checked through a separate pinned attachment |
| `TestTransactionSavepointPartialRollback`, `TestTransactionReadOnlyOption` | `TestTransaction.test_savepoint`, `.test_tpb` | SQL savepoint and Go read-only transaction option; no binary TPB API |
| `TestTransactionSnapshotAndReadCommittedVisibility` | `DatabaseAPI20Test.test_cursor_isolation` | Two pinned attachments verify the distinct post-commit visibility of read-committed and snapshot transactions |
| `TestTransactionNoWaitAndReservationEnforcement` | `TestTransaction.test_tpb` | Native NOWAIT row conflict and exclusive table reservation enforcement, with a bounded child process because lock-wait latency has no hard context-cancellation bound and non-DSQL calls remain non-interruptible |
| `TestConnectorTPBPreservesQuotedIdentifierCase` | Go parity regression | Actual connector-to-native transaction start with a quoted mixed-case table reservation; the reserved table is queried using its case-sensitive identifier |
| `TestEffectiveRoleAuthorization` | Go parity regression based on `TestConnection`/security setup | Owned role/user creation, role grant, no-role denial, and role-selected authorization through separate attachments |
| `TestConstraintViolationReturnsTypedNativeError` | Go parity regression | Duplicate-key failure must retain `*interbase.Error` SQLCODE and native status metadata through `database/sql` |
| `TestTransactionPrepareContextCommit`, `TestTransactionStmtContextRollback`, `TestTransactionErrTxDoneAfterCommitAndRollback` | `TestPreparedStatement.test_execution`; `TestTransaction.test_cursor`; `DatabaseAPI20Test.test_commit`, `.test_rollback` | Additional Go statement/transaction binding and completion requirements |
| `TestTypesIntegerBounds`, `TestTypesCharVarcharUTF8RoundTrip`, `TestTypesUTF8OverlengthRecovery` | `TestInsertData.test_insert_integers`, `.test_insert_char_varchar`; `TestCharsetConversion.testCharVarchar` | Insert/readback, padding, UTF8 character capacity and recovery without truncation |
| `integration/cancellation_test.go`: `TestLiveCancellationRaces`, `TestCanceledImplicitDMLRollsBackAndDoesNotReplay`, `TestCanceledExplicitWritePreservesTransactionOwnership`, `TestCanceledDistributedParticipantRemainsUsable` | Go-specific DSQL cancellation contract | Best-effort prepare/execute/fetch cancellation, authoritative completion-winning races, `isc_cancelled` diagnostics, implicit rollback, explicit-transaction ownership, no `database/sql` replay, pool/statement/cursor reuse, and cancellation cleanup |
| `TestTypesFloatTolerance`, `TestTypesNumericDecimalBindings` | `TestInsertData.test_insert_float_double`, `.test_insert_numeric_decimal` | NaN-safe tolerances, decimal strings/float inputs; no Python Decimal library |
| `TestTypesTimestampPrecisionAndMidnight`, `TestTypesBooleanRoundTrip` | `TestInsertData.test_insert_datetime`, `.test_insert_boolean`; `TestBugs.test_pyib_44` | `time.Time` fractional/midnight values and boolean readback; no distinct Python date object |
| `TestTypesTextAndBinaryBlobRoundTrips`, `TestTypesEmptyBytesDistinguishNull` | `TestInsertData.test_insert_blob`; `TestCharsetConversion.testBlob`; `TestBugs.test_pyib_30`; `DatabaseAPI20Test.test_Binary`, `.test_None` | Text/binary BLOB content, UTF8, embedded NUL, 90,000-byte segments and empty versus NULL; no stream reader or alternate subtype coverage |
| `services_test.go`: `TestServicesManagerInfoAndLog`, `TestServicesAliasesUsersBackupAndRestoreOwnedFixture` | `Connection.get_server_*`, `get_log`, `get_db_alias`, `get_users`, `backup`, `restore`, `create_dump`, and `get_statistics` | Typed native Services Manager requests, bounded job draining, owned alias/user changes, logical backup/restore/dump, and statistics; not full Python Services API parity |
| `TestTypesVarchar5000Insert` | `TestBugs.test_pyib_25` | Actual DDL/insert/readback for lengths 0, 1 and 5000 |
| `direct_test.go`: `TestDirect*` | `TestCursor`, `TestTransaction`, and SQLDA/plan/info native paths | Explicit attachment ownership, scalar/NULL cells, retaining transactions, positioned cursor updates, arrays, streaming BLOB references and parameters, plans, raw info items, and direct completion semantics |
| `change_views_test.go`: `TestDirectChangeViewIndicators*`, `TestDatabaseSQLRejectsChangeViewActivation*` | InterBase ChangeView behavior | Serializable direct-transaction activation, NULL/INSERT/UPDATE/DELETE `Cell.Indicator` flags, and rejection of pooled `database/sql` activation without poisoning the pool |
| `distributed_test.go`: `TestDistributedTransactionCommitsAcrossAttachments` | `TestTransaction` two-phase completion semantics | One native distributed transaction spanning two fixture databases, participant database/transaction info, prepare recovery message, coordinator-only completion, and commit visibility |

## Deferred Coverage

- Python module attributes, exception hierarchy, cursor fetch/map APIs,
  description tuples, `arraysize`, and input/output sizing do not map directly
  to `database/sql`.
- Private DPB/SQLDA fields, version-specific catalog counts, role administration
  beyond the owned effective-authorization test, and explicit raw TPB objects
  remain outside the public `database/sql` surface. Bounded direct plan/info,
  retaining-transaction, cursor-name, and indicator coverage lives in
  `direct_test.go`.
- Stream-BLOB seek/readline APIs, alternate BLOB subtype behavior,
  full Python Services API parity, distributed failure-injection/recovery
  environments, advanced TPB support, cross-client TLS hostname verification,
  platform validation, schema object models,
  backup/restore beyond the Services slice, and encryption administration remain
  separate extension suites. The bounded Services Manager slice is covered by
  `services_test.go`; the direct suite covers
  bounded array round trips and `io.Reader`/`io.ReadCloser` BLOB streaming.
- This is not exhaustive bug-suite parity. The original writable `test_pyib_22`
  sweep is adapted for lengths 0 through 254; other upstream behavior remains
  outside this Go API migration.

## Interpreting Results

The original read-only baseline intentionally rejected Exec, public Prepare, BeginTx,
`time.Time`/`[]byte` arguments, ordinary Go numeric argument conversions, BLOB
results and scaled floating-point results. The implementation now supports
those capabilities. There are no feature skips or expected-failure wrappers;
any failing contract makes the live test command fail.

### Current verification

On 2026-09-18, the serial DSQL-cancellation verification passed with the
temporary Linux/amd64 InterBase client and the requested local image. The Go
and native checks passed as eight Go packages plus ten native ASan/leak
harnesses (`make test`, 19.99s), race (28.90s), checkptr (3.38s), and tagged
integration vet (1.34s). The Bats runner passed **42/42** (11.09s). The live
DSQL cancellation target passed ten race iterations (12.88s test time; 16.68s
including setup/cleanup), the owned fault matrix passed in 16.72s, and native
lifecycle passed in 6.80s. The final four-worker soak ran for
2m0.003813076s and validated 122,283 operations, with 61,143 committed writes,
61,140 rolled-back writes, 61,143 persisted values, 30,573 physical opens and
closes, and 30,572 identity replacements. Its throughput was 1,018.99
operations/s, with 3.925199 ms average and 27.478933 ms maximum latency; both
pools were empty after close, with six file descriptors and two goroutines.
`make build` passed in 0.37s. The exact serial command block, including the
temporary SDK runtime flags, is in
[`docs/production-hardening.md`](../docs/production-hardening.md).

The first unqualified `make test`, race, and vet attempts failed before
testing because the temporary SDK include/runtime paths were not supplied;
reruns with the explicit paths documented above passed. This verification
measures a two-minute soak only and does not add a cross-version cancellation
latency or native-client thread-safety guarantee.

On 2026-09-16, the fresh controller run passed `make test` with the official
SDK (all six Go packages and all ten native ASan/leak harnesses),
`make test-runner BATS=/tmp/opencode/parity-bats/bin/bats` (**21/21**), and
`go vet -tags=integration ./...`. The ordinary full Docker runner also passed,
including the Services framing precheck. Its runners cleaned up only the
resources they owned; a persistent controller-owned
`interbase-go-parity-tests` container was intentionally retained for final root
live checks.

The strict TLS runner is intentionally nonzero because the known hostname
verification defect is a security failure. The race command's first fresh
attempt failed only because pure-Go packages could not load the cgo-linked
`libgds.so` from the explicit SDK location. The fresh rerun with the corrected
environment passed for all six packages, as did checkptr and the root live
tests. The final TLS rerun still reported 12 passing checks and four
wrong-hostname failures. Run the race checks with the matching SDK flags:

```sh
LD_LIBRARY_PATH=/tmp/opencode \
  CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
  CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
  go test -race ./... -count=1 -timeout=120s
```

Earlier migration counts and failures are historical context only, not current
verification.

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

Attribution and permission notices are retained in `UPSTREAM_LICENSE.txt`.
