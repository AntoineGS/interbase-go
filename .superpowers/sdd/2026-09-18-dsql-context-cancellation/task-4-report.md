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

## Fix round 1

### RED

The new native regression harness initially failed before the catalog error
classification fix:

```text
native prepared test failed: ordinary catalog failure did not fall back with an active slot
native prepared test failed: ordinary catalog failure did not fall back with an active slot
```

The first transient-prepare overlap also hung after the native call returned,
showing that the test needed to observe the publication/unpublication wait
before releasing the cancellation call rather than relying only on operation
completion.

### GREEN

- Catalog and procedure metadata now fall back only for ordinary optional
  lookup failures when a cancellation slot is active. `isc_cancelled`, slot
  publication/protocol errors, and all catalog cleanup failures remain
  authoritative and propagate to the caller. Native and cleanup diagnostics
  are preserved together.
- The native harness now blocks `ib_cancel_slot_cancel` across transient
  prepare, exec, procedure `execute2`, catalog `execute2`, and distributed
  participant exec. It asserts publication targeting, waits at the native
  publication barrier, proves cleanup has not started early, then releases
  cancellation and verifies participant rollback recovery.
- Added public root and direct `Exec` cancellation tests while each operation
  owns its connection mutex; both preserve `context.Canceled` and avoid
  `driver.ErrBadConn` classification.

Fresh verification after the fix:

```text
LD_LIBRARY_PATH=/tmp/opencode make test \
  INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
  INTERBASE_LIB=/tmp/opencode                         PASS

LD_LIBRARY_PATH=/tmp/opencode \
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
go test . -run 'Test(RootExecContextCancellationWhileOwningConnectionLock|DirectExecContextCancellationWhileOwningConnectionLock)' \
  -count=1 -v -timeout=60s                              PASS

LD_LIBRARY_PATH=/tmp/opencode \
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
go test -race ./... -count=1 -timeout=180s                PASS

LD_LIBRARY_PATH=/tmp/opencode \
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
go test -race . ./internal/nativegate -run 'Cancel|Cancellation|Uncertain' \
  -count=50 -timeout=180s                                  PASS

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
./scripts/test-integration-docker.sh -v                    PASS

IMAGE=interbase-go-parity-test:local \
INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
./scripts/test-integration-docker.sh --native-lifecycle \
  -run '^TestNativeLifecycleRace$' -v                       PASS
```

## Fix round 2

### RED

The new native regression was run before changing transient preparation. The
successful native prepare was incorrectly allowed to continue after slot
unpublication had failed:

```text
native prepared test failed: successful prepare unpublication failure was not returned
native prepared test failed: transient prepare unpublication failure continued into describe or execute
```

The root/direct lock regressions were also changed from the old pre-operation
`execOverride` seam to an override below a real `nativeCancelOperation`; their
assertions wait for the production watcher to invoke the cancellation slot
while the owning connection mutex is deliberately held.

### GREEN

- `ib_cursor_prepare_statement` now preserves unpublication diagnostics and
  returns failure even when `isc_dsql_prepare` succeeded. Its callers stop
  before statement-type inspection, describe, or execute; native and protocol
  diagnostics remain appended when both fail.
- Added a context-aware native exec test seam that is entered only after slot
  allocation, generation begin, and watcher startup. The root and direct
  cancellation tests now prove one slot cancellation call and watcher-ordered
  close while each operation still owns its connection mutex.
- Extended distributed participant recovery coverage: a canceled participant
  performs a subsequent write with the exact expected persisted value, then
  the same coordinator completes distributed prepare and commit without
  poisoning either attachment.

Fresh round-2 verification:

```text
LD_LIBRARY_PATH=/tmp/opencode \
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
go test -race . ./internal/nativegate \
  -run 'Test(RootExecContextCancellationWhileOwningConnectionLock|DirectExecContextCancellationWhileOwningConnectionLock)$' \
  -count=50 -timeout=180s                                      PASS

LD_LIBRARY_PATH=/tmp/opencode \
INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
INTERBASE_LIB=/tmp/opencode make test-native                      PASS

LD_LIBRARY_PATH=/tmp/opencode \
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
go test . -run 'Distributed' -count=1 -timeout=120s              PASS

IMAGE=interbase-go-parity-test:local \
INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
./scripts/test-integration-docker.sh \
  -run '^TestDistributedTransactionCommitsAcrossAttachments$' -v PASS
```
