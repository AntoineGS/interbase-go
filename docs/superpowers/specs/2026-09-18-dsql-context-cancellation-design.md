# DSQL Context Cancellation Design

**Date:** 2026-09-18
**Status:** Approved design
**Baseline:** `ddddd4c` (`feature/python-parity`)

## Purpose

Add best-effort interruption of active InterBase DSQL operations when their Go
contexts are canceled. The driver must use
`isc_dsql_free_statement(..., DSQL_cancel)` without exposing statement handles
to unsafe reuse, racing native cleanup, or causing `database/sql` to replay a
write whose outcome is not known.

Cancellation covers all SQL statements, including reads and writes. It does not
promise a hard deadline and does not apply to native operations that lack a DSQL
statement handle.

## Evidence and Constraints

The tested InterBase 15.1 client defines `DSQL_cancel` as option `4`. A native
probe using a separate thread and status vector established the following:

- A roughly 3.2-second aggregate fetch returned `isc_cancelled` about 101 ms
  after cancellation was requested.
- A statement waiting for a database row lock returned `isc_cancelled` only
  after roughly 9.2–10 seconds.
- Canceling an idle statement returned success and left the handle reusable.
- A zero-delay race could return cancellation success while the statement still
  completed normally.
- Reuse after the executing call returned and its transaction was rolled back
  succeeded.

Therefore, the cancellation call's return value is not the operation outcome.
The executing prepare/execute/fetch call remains authoritative. The statement
must not be closed, dropped, reused, or freed until both the executing call and
any cancellation request have completed.

The probe evidence is limited to Linux/amd64 client `LI-V15.1.0.42`. It does
not establish a universal latency bound, unrestricted concurrent native-client
usage, or behavior for non-DSQL calls.

## Scope

### Included

- `database/sql` direct and prepared `ExecContext`.
- `database/sql` `QueryContext`, including initial execution and `Rows.Next`.
- `database/sql` `PrepareContext`.
- Direct API transaction execute, query, prepare, and cursor fetch operations.
- Procedure and internal metadata/catalog operations using the shared DSQL
  statement machinery where a caller context is available.
- Explicit cancellation and outcome errors that preserve standard Go context
  matching and native diagnostics.
- Native, unit, race, fault, transaction, and live integration tests.

### Excluded

- Attachment open/create/drop.
- Transaction commit, rollback, retaining operations, and distributed prepare
  or completion after DSQL execution has ended.
- BLOB segment and array-slice I/O.
- Services and Events operations.
- Guaranteed interruption latency or forced process/thread termination.
- Automatic retries of canceled or uncertain writes.

Existing context-aware admission remains responsible for preventing canceled
operations from entering native code while they wait for the process-wide
native gate. This design addresses contexts canceled after DSQL native entry.

## Architecture

### C-Owned Cancellation Slot

The native bridge introduces an opaque cancellation slot owned by C. Go never
reads or stores the raw statement handle. A slot contains:

- a mutex and condition variable;
- the currently published statement-handle address or value;
- a monotonically increasing nonzero generation;
- whether an operation is active;
- the number of cancellation calls currently using the published handle;
- whether cancellation was requested for the active generation;
- the cancellation call's native status, copied into slot-owned storage.

The slot's zero/idle state has no published handle and no active cancel users.
It is allocated before a cancelable operation and destroyed only after the
operation helper has joined its context watcher and native cleanup has removed
the published handle.

### Publication Protocol

Each prepare, execute, or fetch operation follows this protocol inside C:

1. Establish the statement handle and all storage required by the operation.
2. Under the slot mutex, increment the generation, publish the handle, and mark
   the operation active.
3. Execute the blocking InterBase DSQL function.
4. Under the slot mutex, mark the generation inactive and remove the published
   handle.
5. Wait until cancellation users of that generation have returned.
6. Continue native error handling and any close/drop/rollback cleanup.

Publishing occurs immediately before the first cancelable DSQL call. Prepare
publishes after statement allocation and before `isc_dsql_prepare`. Transient
and prepared execution publish before `isc_dsql_execute`/`execute2`. Fetch
publishes before each `isc_dsql_fetch`.

No code may close, drop, replace, or reuse a published statement handle.

### Cancellation Protocol

Go runs the native operation synchronously and starts one watcher for its
context. When `ctx.Done()` closes, the watcher calls a native cancellation
function with the slot and expected generation:

1. Enter the process-wide native gate as an ordinary caller. Cancellation must
   overlap the active ordinary DSQL call; it must not request lifecycle-exclusive
   admission.
2. Under the slot mutex, confirm that the expected generation remains active
   and published.
3. Increment the generation's cancel-user count and copy the statement handle.
4. Release the mutex and call
   `isc_dsql_free_statement(private_status, &handle, DSQL_cancel)`.
5. Reacquire the mutex, record the cancellation-call status for diagnostics,
   decrement the cancel-user count, signal waiters, and leave the gate.

If publication has not occurred, the watcher waits for publication, native
completion, or an explicit operation-done signal. If the operation finishes
first, cancellation becomes a no-op. Generation matching prevents a delayed
watcher from canceling the next execution of a reusable statement.

The cancellation function uses a private status vector. It never uses the
executing operation's status storage and never performs close/drop cleanup.

### Go Operation Helper

A focused Go helper owns each slot and watcher. It:

1. Rejects a context already canceled before native admission.
2. Creates the slot and starts the watcher.
3. Executes the C wrapper synchronously.
4. Signals completion, joins the watcher, and only then destroys the slot.
5. Classifies the executing result, cancellation status, statement type, and
   transaction cleanup result.

The helper does not release object-level mutexes while the native operation is
active. The watcher must not acquire the connection, transaction, statement,
or cursor mutex held by the executing path.

## Result and Error Semantics

The original DSQL operation is authoritative:

| Executing result | Public result |
| --- | --- |
| Success | Success, even if the context was canceled concurrently. |
| `isc_cancelled` after context cancellation | Cancellation error matching the context. |
| Other native error | Preserve the native operation error. |
| Cancellation call fails, operation succeeds | Success; cancellation failure is diagnostic only. |
| Cancellation call fails, operation remains blocked | Continue waiting for the native operation; process supervision remains the hard bound. |

The driver does not report context cancellation merely because the cancellation
request returned success. This avoids converting a completed statement into an
error and prevents applications from retrying successful writes.

### Cancellation Error

An interrupted operation returns a public cancellation error that:

- matches `context.Canceled` or `context.DeadlineExceeded` through `errors.Is`;
- retains the `isc_cancelled` `NativeError` through `errors.As`;
- reports the operation and whether it was read-only or mutating;
- is never `driver.ErrBadConn` solely because cancellation occurred.

### Write Outcomes

Implicit writes already execute in an owned transaction. When execution returns
`isc_cancelled`, the native wrapper rolls that transaction back before returning.
If rollback succeeds, the cancellation error reports that no write was applied.
If rollback fails or the connection is broken before rollback is confirmed, the
driver returns a public typed uncertain-outcome error. That error still matches
the context and retains native execution and cleanup errors.

Explicit transactions remain caller-owned. Live tests must establish, for the
supported client/server stack, that `isc_cancelled` leaves no effect from the
canceled statement and leaves the transaction usable. The driver documents this
tested contract. If the native response or connection state cannot establish
that contract, it returns the typed uncertain-outcome error and leaves explicit
transaction ownership unchanged unless existing connection invalidation rules
require otherwise.

Cancellation never commits or rolls back a caller-owned explicit transaction.
Applications retain responsibility for completing it.

## API-Specific Behavior

### `database/sql`

- Direct and prepared execution use the same cancellation helper.
- A successful execution returns its result despite simultaneous context expiry.
- A cancellation error is not converted to `driver.ErrBadConn`, preventing
  automatic replay.
- Initial query execution is cancelable. Returned rows own a completed cursor,
  not an active watcher.
- Each `Rows.Next` starts and joins its own fetch cancellation watcher.
- A canceled fetch terminates those rows cleanly without dropping or reusing the
  statement before the native fetch returns. Pool usability is tested afterward.
- `PrepareContext` can cancel an active native prepare after statement allocation.

### Direct API

- Direct execute/query/prepare and cursor fetch use the same C slot protocol.
- A canceled cursor remains safely closable after its native call returns.
- Statement and cursor reuse is allowed only after the canceled operation has
  fully joined cancellation and cleanup.

### Internal Catalog Operations

Context-bearing catalog operations adopt the helper where their statement
handle is exposed through the shared wrappers. Contextless mandatory cleanup
remains uninterruptible. The implementation must not invent background contexts
to make internal lifecycle cleanup cancelable.

## Concurrency and Lifecycle Rules

- Execution and its cancellation request may overlap only through the explicit
  cancellation-slot protocol.
- Cancellation uses ordinary native-gate admission. Close, drop, detach, and
  other lifecycle operations retain their existing exclusive/quiescent rules.
- Object cleanup waits for execution and cancellation completion.
- The cancellation watcher never owns cleanup and never mutates transaction
  ownership.
- Every status vector belongs to exactly one concurrent native call.
- Every watcher is joined; no operation leaves a goroutine behind after return.
- Reusable statement generations are monotonic and cannot alias an active prior
  generation during the process lifetime.

## Testing Strategy

### Native deterministic tests

- Cancel active execute, fetch, prepare, and deterministic row-lock wait.
- Complete before cancellation, cancel before publication, and publication versus
  completion races.
- Delay a cancellation request and prove that it cannot affect the next
  generation.
- Race cancellation against close/drop requests and prove cleanup begins only
  after execution and cancellation return.
- Verify independent status vectors and cancellation-error capture.
- Inject cancellation-call failure and preserve the executing result.
- Repeat idle cancellation and statement reuse.

Native race tests use explicit barriers and counters rather than timing-only
assertions. Live probes remain externally bounded because cancellation is not a
hard deadline.

### Transaction tests

- Canceled implicit INSERT/UPDATE/DELETE leaves no persisted effect after the
  wrapper's rollback.
- Canceled explicit writes leave no statement effect, preserve earlier work in
  the transaction, and permit subsequent statements and commit/rollback.
- A later commit cannot reveal partial canceled work.
- Injected rollback failure or connection loss produces the uncertain-outcome
  error and never `driver.ErrBadConn` replay behavior.
- A completed write racing its context deadline returns success exactly once.

### Public API tests

- `errors.Is` matches the originating context error.
- `errors.As` retrieves native cancellation diagnostics and uncertain-outcome
  details where applicable.
- Direct and prepared `database/sql` calls have identical outcome rules.
- Query execution and fetch can each be interrupted.
- Canceled rows close safely and a replacement/retained pool connection works.
- Prepared statements and direct cursors are reusable or safely closable after
  cancellation according to their existing contracts.
- No watcher, file descriptor, statement, cursor, transaction, or attachment
  leaks under repeated cancellation.

### Live acceptance

- CPU-heavy query cancellation returns `isc_cancelled` promptly on the tested
  InterBase stack.
- Row-lock cancellation is observed but is not assigned a strict latency below
  the native client's demonstrated behavior.
- Repeated cancel/complete races run under Go race, checkptr, native ASan/leak
  harnesses, lifecycle churn, the fault matrix, and the concurrent soak.
- All existing ordinary integration, distributed recovery, Services, Events,
  runner, and production-hardening checks remain passing.

## Documentation and Compatibility

The public documentation changes the prior statement that in-flight native SQL
calls cannot be interrupted. It instead states:

- DSQL prepare/execute/fetch use best-effort context cancellation.
- The original native operation may complete successfully despite a concurrent
  cancellation request.
- Some waits can take materially longer than the context deadline.
- Non-DSQL native calls remain non-interruptible.
- Applications must inspect typed uncertainty before retrying writes.

The implementation initially supports the repository's existing Linux/amd64
scope and tested InterBase 15.1 client/server stack. No cross-version latency or
thread-safety guarantee is added.
