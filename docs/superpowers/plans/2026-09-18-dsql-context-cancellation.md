# DSQL Context Cancellation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Interrupt active InterBase DSQL prepare, execute, and fetch calls after Go context cancellation without racing statement cleanup, replaying writes, or canceling a later statement execution.

**Architecture:** C owns an opaque, generation-checked cancellation slot that publishes a statement handle only while a cancelable DSQL call is active. A Go helper watches the operation context and calls `isc_dsql_free_statement(..., DSQL_cancel)` through an ordinary native-gate admission, then joins the watcher before cleanup or reuse. The executing DSQL result remains authoritative; write rollback and uncertainty are classified separately.

**Tech Stack:** Go 1.23, `database/sql/driver`, cgo, C11 with pthread synchronization, InterBase 15.1 native API, Docker integration fixtures, ASan, Go race/checkptr.

**Spec:** `docs/superpowers/specs/2026-09-18-dsql-context-cancellation-design.md`

## Global Constraints

- Baseline is commit `fed5167` on `feature/python-parity`.
- Preserve the existing context-aware `internal/nativegate` admission and callback bridge.
- The executing prepare/execute/fetch result is authoritative; cancellation-call success alone is never reported as proof of cancellation.
- Never close, drop, replace, reuse, or free a published statement handle.
- The cancel call uses an independent status vector and ordinary native-gate admission.
- A completed write returns success even if its context expired concurrently.
- A canceled or uncertain write is never classified as `driver.ErrBadConn` solely because of cancellation.
- Mandatory cleanup remains uninterruptible after native ownership has been acquired.
- No new dependency is permitted.
- Linux/amd64 with the tested InterBase 15.1 client/server remains the supported validation scope.
- Use TDD for every behavior change: observe the intended test fail before production implementation.
- Do not publish the private test image, SDK, client library, fixture credentials, or generated database files.

---

## File Structure

- `native.h`, `native.c`: cancellation-slot ownership, publication, generation, cancel status, and statement-wrapper parameters.
- `native.go`: cgo slot wrapper and context watcher; context-bearing native method signatures.
- `cancellation.go`: public cancellation and uncertain-outcome error contracts.
- `interbase.go`: `database/sql` context propagation and result classification.
- `direct.go`, `distributed.go`: direct transaction, prepared statement, and cursor context propagation.
- `tests/native_cancel_test.c`: deterministic slot/publication/cancel/cleanup native harness.
- `cancellation_test.go`, `direct_context_test.go`, `native_parity_test.go`: unit and public API race/error tests.
- `integration/cancellation_test.go`: live CPU, row-lock, write atomicity, reuse, and pool recovery tests.
- `Makefile`, `README.md`, `docs/production-hardening.md`, `integration/README.md`: repeatable targets and supported semantics.

---

### Task 1: Native Cancellation Slot and Lifetime Protocol

**Files:**
- Modify: `native.h`
- Modify: `native.c`
- Create: `tests/native_cancel_test.c`
- Modify: `Makefile`

**Interfaces:**
- Produces opaque `ib_cancel_slot`.
- Produces `ib_cancel_slot_new`, `ib_cancel_slot_begin`, `ib_cancel_slot_cancel`, and `ib_cancel_slot_free`.
- Produces internal publication/completion helpers used by later statement wrappers.
- `ib_cancel_slot_cancel` returns separate request status and native status without closing or dropping the statement.

- [ ] **Step 1: Add a failing native harness for active cancellation**

Create a harness that stubs `isc_dsql_free_statement`, publishes a fake statement for generation 1, blocks the simulated executing call, requests `DSQL_cancel` on another pthread, and asserts:

```c
assert(cancel_option == DSQL_cancel);
assert(cancel_status_storage != execute_status_storage);
assert(cancel_native_code == 0);
assert(cancel_calls == 1);
```

Also assert that completion cannot proceed to simulated drop until the cancel stub returns.

- [ ] **Step 2: Run the harness and verify RED**

Run:

```sh
cc -std=c11 -Wall -Wextra -Werror -pthread \
  -I/tmp/opencode/interbase-parity-include \
  tests/native_cancel_test.c -o /tmp/opencode/native_cancel_test_red
```

Expected: compilation fails because the slot API does not exist.

- [ ] **Step 3: Implement the slot state machine**

Add an opaque structure with mutex, condition, generation, active/publication flags, statement handle, cancel-user count, operation-complete flag, and copied cancellation status. Implement these externally visible signatures:

```c
typedef struct ib_cancel_slot ib_cancel_slot;

ib_cancel_slot *ib_cancel_slot_new(char **error);
uint64_t ib_cancel_slot_begin(ib_cancel_slot *slot, char **error);
int ib_cancel_slot_cancel(ib_cancel_slot *slot, uint64_t generation,
    int64_t *native_code, char **error);
void ib_cancel_slot_free(ib_cancel_slot *slot);
```

Implement private helpers:

```c
static int ib_cancel_slot_publish(ib_cancel_slot *slot, uint64_t generation,
    isc_stmt_handle *statement, char **error);
static void ib_cancel_slot_complete(ib_cancel_slot *slot, uint64_t generation);
```

`cancel` waits for publication or completion, increments `cancel_users`, copies the handle, releases the mutex during `isc_dsql_free_statement`, records status, decrements users, and signals. `complete` unpublishes and waits for `cancel_users == 0` before returning.

- [ ] **Step 4: Add race and no-op native cases**

Cover:

- cancel before publication;
- operation completes before cancel;
- idle/no-active generation;
- stale generation after a second `begin`;
- cancel failure status;
- delayed cancel versus completion;
- allocation failure;
- free only after operation and cancel completion.

- [ ] **Step 5: Run GREEN native checks**

Run the harness with pthreads, then ASan/leak detection:

```sh
cc -std=c11 -Wall -Wextra -Werror -pthread \
  -I/tmp/opencode/interbase-parity-include \
  tests/native_cancel_test.c -o /tmp/opencode/native_cancel_test
/tmp/opencode/native_cancel_test

cc -std=c11 -Wall -Wextra -g -O1 -fsanitize=address \
  -fno-omit-frame-pointer -pthread \
  -I/tmp/opencode/interbase-parity-include \
  tests/native_cancel_test.c -o /tmp/opencode/native_cancel_test_asan
ASAN_OPTIONS=detect_leaks=1 /tmp/opencode/native_cancel_test_asan
```

Expected: both pass with no warnings or sanitizer findings.

- [ ] **Step 6: Add the harness to `make test-native` and commit**

Add a compile/run recipe following the ten existing harnesses. Commit:

```sh
git add native.h native.c tests/native_cancel_test.c Makefile
git commit -m "Add native DSQL cancellation slot"
```

---

### Task 2: Go Watcher and Public Error Contracts

**Files:**
- Create: `cancellation.go`
- Create: `cancellation_test.go`
- Modify: `native.go`
- Modify: `native_parity_test.go`

**Interfaces:**
- Consumes the Task 1 C slot API.
- Produces `CancellationError` and `UncertainOutcomeError`.
- Produces internal `nativeCancelOperation` with `begin`, `watch`, `finish`, and `close` ownership.
- Produces classification helpers used by `database/sql` and direct APIs.

- [ ] **Step 1: Write failing public error tests**

Specify these contracts:

```go
var cancelErr *CancellationError
if !errors.Is(err, context.DeadlineExceeded) { t.Fatal(...) }
if !errors.As(err, &cancelErr) { t.Fatal(...) }
var nativeErr *NativeError
if !errors.As(err, &nativeErr) { t.Fatal(...) }
if errors.Is(err, driver.ErrBadConn) { t.Fatal(...) }
```

For uncertain writes, also require:

```go
var uncertain *UncertainOutcomeError
if !errors.As(err, &uncertain) || !uncertain.Mutating { t.Fatal(...) }
```

- [ ] **Step 2: Run RED public tests**

Run:

```sh
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test . -run 'Test(Cancellation|UncertainOutcome)Error' -count=1
```

Expected: compilation fails because the error types do not exist.

- [ ] **Step 3: Implement error types**

Define:

```go
type CancellationError struct {
    Operation string
    Mutating  bool
    Context   error
    Native    error
}

func (e *CancellationError) Error() string
func (e *CancellationError) Unwrap() []error

type UncertainOutcomeError struct {
    Operation string
    Mutating  bool
    Cause     error
    Cleanup   error
}

func (e *UncertainOutcomeError) Error() string
func (e *UncertainOutcomeError) Unwrap() []error
```

Messages must not include SQL text, bindings, credentials, or attachment strings.

- [ ] **Step 4: Write failing watcher race tests**

Use native test overrides/barriers to prove:

- pre-canceled context never begins a slot;
- cancellation waits for publication;
- operation completion wins a simultaneous race and returns success;
- `isc_cancelled` plus context returns `CancellationError`;
- a different native error remains authoritative;
- cancellation-call failure does not replace successful execution;
- watcher is joined before slot destruction;
- a delayed watcher cannot target a new generation.

- [ ] **Step 5: Implement `nativeCancelOperation`**

Use a completion channel and exactly one watcher. The watcher enters
`nativegate.Global.Enter()` before calling the C cancel API, does not acquire an
object mutex, and reports request diagnostics to the owner. `finish` closes the
completion signal, waits for the watcher, and then frees the slot.

- [ ] **Step 6: Run race tests and commit**

Run:

```sh
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test -race . ./internal/nativegate -run 'Cancel|Cancellation|Uncertain' \
  -count=50 -timeout=180s
```

Commit:

```sh
git add cancellation.go cancellation_test.go native.go native_parity_test.go
git commit -m "Add context cancellation watcher"
```

---

### Task 3: Prepared Statement and Cursor Fetch Cancellation

**Files:**
- Modify: `native.h`
- Modify: `native.c`
- Modify: `native.go`
- Modify: `interbase.go`
- Modify: `direct.go`
- Modify: `driver_test.go`
- Modify: `direct_context_test.go`
- Modify: `tests/native_prepared_test.c`
- Modify: `tests/native_values_test.c`

**Interfaces:**
- Consumes `nativeCancelOperation` and the C slot.
- Changes prepared execution/query and cursor fetch native methods to accept `context.Context`.
- Preserves contextless driver methods by calling context-aware implementations with `context.Background()`.

- [ ] **Step 1: Add RED tests for prepared execute and fetch**

Test public `sql.Stmt.ExecContext`, `sql.Stmt.QueryContext`, direct prepared statements, and `Rows.Next`/direct cursor `Next`. Hold the simulated native call behind a barrier, cancel the context, return `isc_cancelled`, and assert context/native error identity, no `ErrBadConn`, and no close/drop before the worker exits.

- [ ] **Step 2: Add RED native prepared/fetch publication tests**

Assert publication immediately before `isc_dsql_execute`, `execute2`, and `fetch`; assert unpublication and cancel join before existing cleanup paths.

- [ ] **Step 3: Thread slot and generation through prepared wrappers**

Extend:

```c
int ib_statement_exec(..., ib_cancel_slot *cancel, uint64_t generation, ...);
ib_cursor *ib_statement_query(..., ib_cancel_slot *cancel,
    uint64_t generation, ...);
int ib_cursor_next(ib_cursor *cursor, ib_cancel_slot *cancel,
    uint64_t generation, char **error);
```

Publish the address of the actual `isc_stmt_handle` used by each call. Complete
the slot before any rollback, cursor abort, statement close, or drop.

- [ ] **Step 4: Propagate contexts through Go prepared/cursor paths**

Change native signatures to:

```go
func (s *nativeStatement) exec(ctx context.Context, args []argument) (int64, error)
func (s *nativeStatement) query(ctx context.Context, args []argument) (*nativeCursor, []string, error)
func (c *nativeCursor) next(ctx context.Context) (bool, error)
```

Update `stmt.ExecContext`, `stmt.QueryContext`, `rows.Next`, direct statement
methods, and direct cursor methods. Preserve the existing known-success rule.

- [ ] **Step 5: Verify statement reuse and cleanup**

Add tests that cancel one generation, reuse the prepared statement successfully,
cancel a fetch, close its rows/cursor, and execute another statement on the same
connection/transaction.

- [ ] **Step 6: Run focused suites and commit**

Run Go race tests plus native prepared/value harnesses. Commit:

```sh
git add native.h native.c native.go interbase.go direct.go driver_test.go \
  direct_context_test.go tests/native_prepared_test.c tests/native_values_test.c
git commit -m "Cancel prepared execution and fetch"
```

---

### Task 4: Transient Prepare, Query, and Execute Cancellation

**Files:**
- Modify: `native.h`
- Modify: `native.c`
- Modify: `native.go`
- Modify: `interbase.go`
- Modify: `direct.go`
- Modify: `distributed.go`
- Modify: `driver_test.go`
- Modify: `direct_context_test.go`
- Modify: `distributed_test.go`
- Modify: `tests/native_direct_test.c`
- Modify: `tests/native_lifecycle_test.c`

**Interfaces:**
- Extends connection and transaction query/exec/prepare wrappers with slot and generation.
- All context-bearing root/direct/distributed DSQL entry points use the shared helper.
- Commit, rollback, retaining, BLOB, array, Services, and Events interfaces remain unchanged.

- [ ] **Step 1: Add RED tests for transient operations**

Cover connection and explicit/distributed participant transaction execute,
query, prepare, and procedure execution. Assert cancellation can run while the
caller holds connection/transaction mutexes and that cleanup waits for both
native calls.

- [ ] **Step 2: Extend C transient wrapper signatures**

Add slot/generation parameters to:

```c
ib_connection_query
ib_connection_exec
ib_statement_prepare
ib_transaction_query
ib_transaction_exec
ib_transaction_prepare
```

Publish around `isc_dsql_prepare`, `isc_dsql_execute`, and `execute2`, including
catalog/procedure paths reached by these wrappers. Do not publish around commit,
rollback, BLOB, or array calls.

- [ ] **Step 3: Propagate `context.Context` in native Go methods**

Change connection/transaction methods to take context explicitly. Replace each
old call site with its public operation context; contextless compatibility
methods use `context.Background()`.

- [ ] **Step 4: Preserve lifecycle and distributed ownership**

Test canceled distributed participant execution, subsequent participant cleanup,
and no interference with distributed prepare/commit recovery. Re-run the
existing canceled-Prepare cursor-ownership regression.

- [ ] **Step 5: Run focused and lifecycle tests**

Run root/direct/distributed tests under race, native direct/lifecycle harnesses,
and the opt-in lifecycle Docker test once.

- [ ] **Step 6: Commit**

```sh
git add native.h native.c native.go interbase.go direct.go distributed.go \
  driver_test.go direct_context_test.go distributed_test.go \
  tests/native_direct_test.c tests/native_lifecycle_test.c
git commit -m "Cancel transient DSQL operations"
```

---

### Task 5: Transactional Write Outcomes and Replay Safety

**Files:**
- Modify: `native.c`
- Modify: `native.go`
- Modify: `interbase.go`
- Modify: `direct.go`
- Modify: `cancellation.go`
- Modify: `cancellation_test.go`
- Create: `integration/cancellation_test.go`

**Interfaces:**
- Consumes statement cancellation across all SQL paths.
- Produces confirmed rollback versus uncertain outcome classification.
- Preserves caller ownership of explicit transactions.

- [ ] **Step 1: Add live RED tests for implicit writes**

Use a trigger or expensive DML expression that remains active long enough to
cancel. Assert canceled INSERT, UPDATE, and DELETE return a context-matching
error and leave no persisted effect after the driver's implicit rollback.

- [ ] **Step 2: Add live RED tests for explicit writes**

Within one explicit transaction:

1. persist an earlier control change;
2. cancel an expensive mutating statement;
3. verify its target effect is absent inside the same transaction;
4. execute another statement;
5. commit and verify only the control and later statement persist.

Repeat with rollback instead of commit.

- [ ] **Step 3: Implement outcome classification**

When an implicit write returns `isc_cancelled`, require confirmed successful
rollback before returning a plain `CancellationError`. Join rollback failure,
broken connection, or lost response into `UncertainOutcomeError`. Explicit
transactions return cancellation while retaining ownership when native state
remains usable; contradictory state becomes uncertain.

- [ ] **Step 4: Prove no `database/sql` replay**

Use a non-idempotent generator/log effect and operation marker. Assert exactly
one execution attempt for completion races and no duplicate effect for canceled
or uncertain writes. Assert cancellation errors never match `driver.ErrBadConn`.

- [ ] **Step 5: Add fault-injection cleanup cases**

Inject cancel-call failure, rollback failure, allocation failure, and connection
loss after cancellation request. Assert error trees preserve context, native,
and cleanup diagnostics without leaking credentials or SQL values.

- [ ] **Step 6: Run integration tests and commit**

Run focused unit/race tests and the new integration cancellation suite against
the owned Docker fixture. Commit:

```sh
git add native.c native.go interbase.go direct.go cancellation.go \
  cancellation_test.go integration/cancellation_test.go
git commit -m "Classify canceled write outcomes"
```

---

### Task 6: Repeated Live Cancellation and Resource Safety

**Files:**
- Modify: `integration/cancellation_test.go`
- Modify: `integration/soak_test.go`
- Modify: `scripts/test-integration-docker.sh`
- Modify: `tests/runner/test_runner.bats`
- Modify: `Makefile`

**Interfaces:**
- Adds explicit `--cancellation` runner opt-in and `make test-cancellation`.
- Adds configurable cancellation iterations without weakening ordinary tests.

- [ ] **Step 1: Add runner RED tests**

Require an allowlisted cancellation flag, bounded positive iteration count, no
ambient environment inheritance, sufficient external timeout, and owned-fixture
cleanup on interruption.

- [ ] **Step 2: Implement runner controls**

Add:

```text
--cancellation
--cancellation-iterations=N   # 1..10000
```

Forward only `INTERBASE_CANCELLATION=1` and the validated iteration count to the
requested integration binary.

- [ ] **Step 3: Add repeated live race tests**

Repeat CPU query cancellation, completion races, prepared reuse, fetch
cancellation, implicit/explicit writes, and pool recovery. Sample goroutines,
file descriptors, DB pool counters, and server attachment counts where available.

- [ ] **Step 4: Extend soak with bounded cancellation traffic**

When the explicit cancellation opt-in is set, allocate a fraction of workers to
cancel DSQL operations while ordinary transactions and physical churn continue.
Verify committed counters, rolled-back counters, prepared reuse, and cleanup.

- [ ] **Step 5: Run runner tests and live cancellation stress**

Run Bats, ShellCheck, the cancellation target, lifecycle race, fault matrix, and
a 120-second cancellation-enabled soak.

- [ ] **Step 6: Commit**

```sh
git add integration/cancellation_test.go integration/soak_test.go \
  scripts/test-integration-docker.sh tests/runner/test_runner.bats Makefile
git commit -m "Add repeated DSQL cancellation validation"
```

---

### Task 7: Documentation, Independent Review, and Final Verification

**Files:**
- Modify: `README.md`
- Modify: `docs/production-hardening.md`
- Modify: `integration/README.md`

**Interfaces:**
- Documents exact supported operations, error inspection, latency limits, and commands.
- Leaves the previously deferred feature list intact.

- [ ] **Step 1: Update execution semantics**

Replace the blanket non-interruptible SQL statement with the exact contract:
best-effort DSQL prepare/execute/fetch cancellation, authoritative executing
result, potentially slow lock-wait interruption, and unchanged non-DSQL limits.

- [ ] **Step 2: Add usage examples**

Show `errors.Is`/`errors.As` handling for cancellation and uncertain writes,
including the rule that callers reconcile uncertainty before retrying.

- [ ] **Step 3: Obtain independent code review**

Review native handle lifetime, generation races, gate admission, transaction
outcomes, `database/sql` replay behavior, C/Go memory ownership, error redaction,
and runner cleanup. Resolve all P1/P2 findings with regressions.

- [ ] **Step 4: Run fresh complete verification**

Run serially with the official SDK/client environment:

```sh
make test INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include INTERBASE_LIB=/tmp/opencode
go test -race ./... -count=1 -timeout=300s
go test -gcflags=all=-d=checkptr=2 ./... -count=1 -timeout=180s
go vet -tags=integration ./...
make test-runner BATS=/tmp/opencode/parity-bats/bin/bats
make test-cancellation IMAGE=interbase-go-parity-test:local INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include
make test-faults IMAGE=interbase-go-parity-test:local INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include
make test-native-lifecycle IMAGE=interbase-go-parity-test:local INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include
make test-soak IMAGE=interbase-go-parity-test:local INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include SOAK_DURATION=120s
make build INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include INTERBASE_LIB=/tmp/opencode
git diff --check
```

Use `LD_LIBRARY_PATH=/tmp/opencode` for Go race/checkptr commands linked to the
temporary client. Record actual output and durations; do not claim a hard
deadline or multi-hour endurance unless measured.

- [ ] **Step 5: Commit documentation**

```sh
git add README.md docs/production-hardening.md integration/README.md
git commit -m "Document DSQL context cancellation"
```

- [ ] **Step 6: Inspect final history and worktree**

Confirm the original checkout remains untouched except for its pre-existing
user files, the feature worktree contains only intentional untracked planning
artifacts, and no owned disposable cancellation containers remain.
