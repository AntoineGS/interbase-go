# Task 5 Report: Transactional Write Outcomes and Replay Safety

## Result

Task 5 is implemented on top of Task 4's transient DSQL cancellation support.
The intended commit message is:

```text
Classify canceled write outcomes
```

Canceled mutating operations now distinguish confirmed implicit rollback and a
usable caller-owned transaction from an unknown outcome.  Unknown state is
reported through `UncertainOutcomeError`; no canceled write is converted to
`driver.ErrBadConn`, so `database/sql` cannot safely replay it.

## Implementation

### Native outcome tracking

- Added a native write-outcome state to each connection view:
  `unknown`, `rollback confirmed`, and `explicit transaction usable`.
- Failed DSQL cleanup records the state only after cursor cleanup and implicit
  rollback have completed.  A cleanup failure or broken connection remains
  unknown.
- Exposed outcome accessors for root connections, explicit transactions, and
  prepared statements.
- Kept the executing DSQL result authoritative; the cancellation request is
  retained only as a diagnostic when the executing call itself fails.

### Go classification and diagnostics

- Added `classifyNativeWriteOutcome`, which requires a known post-cancellation
  state for mutating operations.
- Implicit writes with confirmed rollback return `CancellationError`.
- Explicit writes with a usable transaction return `CancellationError` while
  leaving transaction completion to the caller.
- Failed rollback, broken/lost connection, and unknown native state return
  `UncertainOutcomeError` while retaining context, native, and cleanup error
  identities through `errors.Is`/`errors.As`.
- Split combined native execution/cleanup diagnostics before sanitization, and
  propagated failed cancellation-request diagnostics without changing a known
  successful operation into an error.
- Applied the classification consistently to root, prepared, direct, and
  explicit-transaction execution paths.

## Live integration fixture

Created `integration/cancellation_test.go` with an owned schema rather than
reusing the ordinary fixture.  The schema contains:

- A trigger-backed target table for expensive INSERT, UPDATE, and DELETE.
- A stored procedure that scans 4,096 rows for 10,000 passes, keeping the DSQL
  operation active long enough for native cancellation.
- A transactional log table and a non-transactional generator marker.  The
  generator proves exactly one execution attempt without relying on an
  idempotent row effect.

The live tests prove:

- Implicit INSERT, UPDATE, and DELETE return context-matching
  `CancellationError`, preserve the original persisted row state, leave no log
  row, and leave the pooled attachment usable for a later write.
- Explicit transactions retain ownership after a canceled INSERT.  A prior
  control write and a later control write persist on commit; both disappear on
  rollback; the canceled target and log rows never persist.
- A distributed participant can be canceled, used for a subsequent write,
  prepared, committed, and read back through a new attachment.  The second
  participant also commits normally.
- Canceled errors contain the `isc_cancelled` native code, match
  `context.Canceled`, do not match `driver.ErrBadConn`, and are not uncertain
  on the healthy tested stack.

The distributed scenario succeeded on the required InterBase Docker image, so
there is no live distributed blocker to report.

The cancellation environment forwarding flag belongs to Task 6's runner work.
For this task's live proof, a short-lived local forwarding line was used to
pass `INTERBASE_CANCELLATION=1` into the already-owned Docker test binary; the
runner file was restored afterward and has no uncommitted change.  Ordinary
integration runs therefore continue to skip these deliberately expensive
contracts until Task 6 adds the explicit runner opt-in.

## Tests added or extended

- `TestClassifyNativeWriteOutcomeRequiresKnownCleanup` covers confirmed
  rollback, usable explicit state, and unknown state.
- `TestNativeExecutionErrorPreservesPrimaryAndCleanupDiagnostics` verifies the
  native and cleanup error tree.
- The live cancellation suite covers all implicit DML types, explicit commit
  and rollback, connection reuse, generator/log replay markers, and
  distributed participant recovery.
- Existing native cancellation, lifecycle, direct, prepared, distributed, and
  leak-detection harnesses were rerun.

## TDD and debugging evidence

The Task 5 unit RED cycle initially failed at compile time because the new
write-outcome symbols and accessors were undefined.  After the native and Go
classification implementation, the focused tests passed.  The first live
fixture attempt also failed as expected during fixture provisioning on the
reserved SQL alias `SECOND`, and the next attempt exposed that this tested
Dialect 3 stack does not accept `CROSS JOIN`; the fixture was corrected to use
non-reserved aliases and comma joins before the behavior assertions were run.

## Fresh verification

All commands below were run against the current worktree with the temporary
InterBase SDK/client at `/tmp/opencode` and the required Docker image
`interbase-go-parity-test:local`:

```text
LD_LIBRARY_PATH=/tmp/opencode \
INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
INTERBASE_LIB=/tmp/opencode make test                         PASS

CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test -race ./... -count=1 -timeout=300s                         PASS

CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test -gcflags=all=-d=checkptr=2 ./... -count=1 -timeout=180s PASS

CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go vet -tags=integration ./...                                  PASS

CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test -race . ./internal/nativegate \
  -run 'Cancel|Cancellation|Uncertain' -count=50 -timeout=180s  PASS

IMAGE=interbase-go-parity-test:local \
INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
./scripts/test-integration-docker.sh -v                         PASS

```

With the short-lived Task 6 forwarding line and
`INTERBASE_CANCELLATION=1`, the three live commands were run separately as:

```text
./scripts/test-integration-docker.sh \
  -run '^TestCanceledImplicitDMLRollsBackAndDoesNotReplay$' -v -timeout=5m PASS
./scripts/test-integration-docker.sh \
  -run '^TestCanceledExplicitWritePreservesTransactionOwnership$' -v -timeout=5m PASS
./scripts/test-integration-docker.sh \
  -run '^TestCanceledDistributedParticipantRemainsUsable$' -v -timeout=5m PASS
```

The ordinary Docker run passed all enabled integration contracts; its
opt-in-only cancellation, fault, soak, lifecycle-race, and TLS cases remained
skipped as designed.  Native ASan/leak harnesses reported no sanitizer or leak
diagnostics.  `git diff --check` also passed.  The pre-existing untracked
planning/specification files were not staged.

## Fix round 1

### Review findings addressed

- Cancellation slots now return separate `attempted` and `overlapped` evidence.
  The native bridge reports `overlapped` only after it observes a published
  active statement and enters the native cancel call.
- The executing error remains authoritative.  A transport or lost-response
  error is classified as a canceled write only when overlap evidence exists;
  an expired context without overlap remains the original native error.
- Cancellation-request diagnostics are stored separately from the executing
  error, so an `isc_cancelled` request diagnostic cannot relabel an ordinary
  execution failure.  Failed request, execution, and cleanup diagnostics remain
  in the sanitized error tree.
- Outcome-state parsing is conservative for unknown native values.  Broken
  connections and failed rollback cleanup remain `UncertainOutcomeError` and
  never become `driver.ErrBadConn`.

### TDD evidence

The RED cycle first failed to compile because the new cancellation evidence
type, fake-slot fields, and allocation seam were undefined:

```text
go test . -run 'Test(ClassifyNativeWriteOutcomeUsesOverlapping|ClassifyNativeWriteOutcomeDoesNotInfer|ExecContextOverlappingLostResponse|ExecContextCancellationRequestFailureWithoutOverlap|ExecContextSlotAllocationFailure)' -count=1
FAIL: undefined nativeCancellationEvidence, cancelAttempted,
      cancelOverlapped, and nativeCancelSlotFactory
```

The outcome-state parsing test then independently failed RED on its undefined
parser helper.  After the minimal native/Go implementation, the focused
classifier, public `ExecContext`, database/sql replay, and parser tests passed.

### Fault and replay coverage

- Cancellation-call failure with an overlapping request is exercised through
  the public `conn.ExecContext` path.
- Rollback/cleanup failure is injected through the native execution error path;
  the test asserts `CancellationError`, `UncertainOutcomeError`, context,
  primary native, cleanup, and request identities, plus redacted rendering.
- Slot allocation failure is injected at the native operation factory and
  proves execution is never entered.
- Connection loss is injected through the public execution path with a broken
  native connection; the result is uncertain and the connection is invalidated.
- A successful completion/cancellation race returns the authoritative result
  with exactly one non-idempotent execution attempt.
- A lost-response uncertainty is run through `database/sql`; it matches the
  context, does not match `driver.ErrBadConn`, and is observed exactly once,
  proving no automatic replay.

### Fix-round verification

```text
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test ./... -count=1 -timeout=300s                              PASS

CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test -race . -run 'Cancel|Cancellation|Uncertain|Replay|Outcome' \
  -count=20 -timeout=180s                                         PASS

CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test -race ./... -count=1 -timeout=300s                         PASS

CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test -gcflags=all=-d=checkptr=2 ./... -count=1 -timeout=180s PASS

CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go vet -tags=integration ./...                                  PASS

INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
INTERBASE_LIB=/tmp/opencode make test-native                       PASS

# Focused Docker run used a temporary, uncommitted copy of the current
# runner with a literal INTERBASE_CANCELLATION=1 Docker-exec environment
# entry; the repository runner was not changed.  The temporary directory was
# removed after the run.
tmp_runner_dir="$(mktemp -d "${PWD}/.tmp-cancellation-runner.XXXXXX")"
cp scripts/test-integration-docker.sh "${tmp_runner_dir}/test-integration-docker.sh"
sed -i "/--env SERVICES_NATIVE_TRACE/a\\    --env 'INTERBASE_CANCELLATION=1'" \
  "${tmp_runner_dir}/test-integration-docker.sh"
chmod +x "${tmp_runner_dir}/test-integration-docker.sh"
IMAGE=interbase-go-parity-test:local \
INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
"${tmp_runner_dir}/test-integration-docker.sh" \
  -run '^TestCanceled(ImplicitDMLRollsBackAndDoesNotReplay|ExplicitWritePreservesTransactionOwnership|DistributedParticipantRemainsUsable)$' \
  -v -timeout=5m                                               PASS
rm -rf "${tmp_runner_dir}"
```
