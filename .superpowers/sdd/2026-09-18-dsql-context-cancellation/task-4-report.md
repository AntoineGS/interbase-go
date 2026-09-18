# Task 4 Report: Transient Prepare, Query, and Execute Cancellation

## Result

Task 4 is implemented on top of `b9ad2cb` (`Fix Task 3 cancellation review
findings`). The required commit message is:

```text
Cancel transient DSQL operations
```

The implementation extends best-effort context cancellation to transient
prepare, execute, query, procedure, catalog, and distributed-participant DSQL
paths while preserving the Task 3 slot lifecycle and authoritative native
result rules.

## Implementation

### Native transient wrappers

- Extended connection and transaction query/execute/prepare wrappers with an
  opaque cancellation slot and generation.
- Published the actual statement handle immediately before transient
  `isc_dsql_prepare`, `isc_dsql_execute`, and `isc_dsql_execute2` calls.
- Added handle republishing for procedure and relation metadata catalog calls;
  catalog fetches keep the outer operation active while ordinary fetches retain
  their completion semantics.
- Kept array metadata and mandatory cleanup paths contextless, and did not add
  cancellation to commit, rollback, retaining, BLOB, Services, or Events
  operations.
- Ensured slot completion/unpublication precedes cursor, statement,
  transaction, and connection cleanup.

### Go and public API propagation

- Changed native connection and transaction transient DSQL methods to accept
  the caller's `context.Context` and create one shared cancellation operation
  per native entry point.
- Propagated contexts through `database/sql` connection, prepared statement,
  direct transaction, direct plan, and cursor paths.
- Preserved `context.Background()` only for contextless compatibility methods.
- Kept successful native results authoritative when cancellation is observed
  after the native operation completed.
- Classified executing `isc_cancelled` results through the existing typed
  cancellation errors without converting cancellation to `driver.ErrBadConn`.
- Distributed participants use the same context-aware transaction paths; the
  coordinator's distributed prepare/commit/rollback completion remains
  non-cancelable.

## Tests added or extended

- Native direct publication checks for transient prepare, execute, and
  execute2/query paths, including actual statement-handle publication and
  cleanup ordering.
- Native compatibility call sites for lifecycle, plan, and prepared harnesses.
- Existing public prepared/direct cancellation and distributed ownership
  regressions were rerun against the transient implementation.

No public direct prepared-statement API was added.

## TDD/debugging evidence

The opt-in native lifecycle race provided a reproducible RED regression after
the initial implementation:

```text
worker 1 iteration 0: interbase: query prepared statement:
cancellation slot generation cannot be published
```

Root cause: prepared-query metadata republished the slot to the prepared
statement, but the subsequent prepared `execute2` path used the stricter
one-shot publication helper and rejected the already-published handle. The
prepared query path now uses the idempotent republish helper, matching the
transient query path.

The same lifecycle command then passed:

```text
--- PASS: TestNativeLifecycleRace (3.18s)
PASS
```

## Fresh verification

All commands below were run against the current worktree using the temporary
InterBase SDK/client at `/tmp/opencode`:

```text
LD_LIBRARY_PATH=/tmp/opencode make test \
  INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
  INTERBASE_LIB=/tmp/opencode                         PASS

LD_LIBRARY_PATH=/tmp/opencode \
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
go test -race ./... -count=1 -timeout=180s                  PASS

LD_LIBRARY_PATH=/tmp/opencode \
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
go test -race . ./internal/nativegate -run 'Cancel|Cancellation|Uncertain' \
  -count=50 -timeout=180s                                   PASS

LD_LIBRARY_PATH=/tmp/opencode \
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
go test -gcflags=all=-d=checkptr=2 ./... -count=1 -timeout=180s PASS

LD_LIBRARY_PATH=/tmp/opencode \
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
go vet ./...                                             PASS

IMAGE=interbase-go-parity-test:local \
INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
./scripts/test-integration-docker.sh --native-lifecycle \
  -run '^TestNativeLifecycleRace$' -v                       PASS

IMAGE=interbase-go-parity-test:local \
INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
./scripts/test-integration-docker.sh -v                    PASS

git diff --check                                         PASS
```

`make test` passed all Go packages and all native ASan/leak-detection
harnesses, including direct, lifecycle, prepared, distributed, Services,
Events, and cancellation tests. The full Docker integration run passed its
enabled tests; its explicitly opt-in fault, soak, TLS, and lifecycle-race
cases remained skipped in that ordinary run and the lifecycle race was run
separately above.

The pre-existing untracked planning/specification files were not staged.
