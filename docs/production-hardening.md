# Production hardening

## Execution contract

`Config.ConnectTimeout` sets the native attachment connection timeout. Zero
keeps the client default; positive durations round upward to whole seconds.
Negative durations and values exceeding the native unsigned 32-bit seconds
field are rejected. This is an attachment timeout, not a query deadline.

Context cancellation prevents execution when observed before native entry. For
DSQL prepare, execute, and fetch operations, cancellation after native entry is
best effort: the driver requests native DSQL cancellation, then waits for the
executing call and its cleanup to finish. The executing result is authoritative.
A successful `database/sql` execution therefore returns its affected-row result
even if cancellation arrives during that call; returning an error would make
`database/sql` discard the result and could invite an unsafe retry. A successful
cancellation request is not itself an operation result. An executing
`isc_cancelled` result after context cancellation is reported as a typed
cancellation error. If there is no proven overlapping cancellation, another
native error remains the authoritative native operation error. If the watcher
proves that cancellation overlapped an active DSQL call but the response is
lost, the driver may instead report context cancellation or typed
`UncertainOutcomeError` even when the native error is not `isc_cancelled`; the
error tree retains the native execution and cleanup diagnostics. DSQL
cancellation is never converted to `driver.ErrBadConn` or replayed.

The supported DSQL entry points are `database/sql` `PrepareContext`, direct and
prepared `ExecContext`, `QueryContext`/`QueryRowContext` and each `Rows.Next`,
as well as direct `Transaction.Query`, `Transaction.Exec`, `Transaction.Plan`,
and `Cursor.Next`. Procedures and context-bearing catalog work are covered when
they use these DSQL paths. The watcher is joined before a rows, cursor, or
statement handle is closed or reused.

An error after a lost response does not prove a write failed. A canceled write
with confirmed rollback (implicit transaction) or confirmed usable explicit
transaction returns `CancellationError`; if rollback or final state cannot be
confirmed, the driver returns `UncertainOutcomeError`. Applications must
inspect these errors with `errors.Is`/`errors.As`, reconcile an uncertain result
using their own operation identifier or database state, and never blindly retry
the write. Explicit transaction execution success does not imply that its later
commit succeeded. Cancellation never commits or rolls back a caller-owned
explicit transaction; the caller remains responsible for completing it.

For example:

```go
_, err := db.ExecContext(ctx, query, args...)
if err == nil {
    return nil
}

var uncertain *interbase.UncertainOutcomeError
if errors.As(err, &uncertain) {
    // Reconcile the operation ID or database state before deciding on a retry.
    return err
}

var canceled *interbase.CancellationError
if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) &&
    errors.As(err, &canceled) {
    var native *interbase.NativeError
    if errors.As(err, &native) {
        log.Printf("DSQL cancellation status=%d", native.NativeCode)
    }
    return err
}
return err
```

DSQL cancellation has no hard latency limit. On the tested Linux/amd64
InterBase 15.1 `LI-V15.1.0.42` client, a roughly 3.2-second aggregate fetch
returned `isc_cancelled` about 101 ms after cancellation was requested. A
statement waiting for a database row lock returned only after roughly 9.2–10
seconds. These measurements are evidence for that client/server stack, not a
portable bound; process supervision remains the hard bound for a stuck call.

Non-DSQL native calls remain non-interruptible after entry. This includes
attachment open/create/drop, transaction begin/commit/rollback/retaining and
distributed completion, BLOB segment/array I/O, Services, and Events. Their
existing context admission and cleanup behavior is unchanged; Event wait
cancellation is a separate subscription contract, not DSQL cancellation.

`TransactionOptions.NoWait` avoids waiting for conflicting database locks.
It is not a general statement or network timeout. Process supervision remains
necessary for a hard bound on a stuck native call.

## Repeatable checks

Supply the matching licensed SDK and an existing private test image. The live
runners create disposable databases/containers; no application database is
needed. For a nonstandard client location, configure its runtime search path
as well as the build paths, for example:

```sh
export LD_LIBRARY_PATH=/path/to/client/lib
make test INTERBASE_INCLUDE=/path/to/sdk/include INTERBASE_LIB=/path/to/client/lib
```

Hardening entry points:

```sh
# Real TCP response loss, blackhole, stale pool and owned-server restart tests.
make test-faults IMAGE="$IMAGE" INTERBASE_INCLUDE=/path/to/sdk/include

# Concurrent work, commit/rollback correctness, early rows.Close, physical churn.
make test-soak IMAGE="$IMAGE" INTERBASE_INCLUDE=/path/to/sdk/include \
  SOAK_DURATION=120s SOAK_WORKERS=4 SOAK_SAMPLE_INTERVAL=1s

# Longer runs use the same workload; duration is not a claim of prior verification.
make test-soak IMAGE="$IMAGE" INTERBASE_INCLUDE=/path/to/sdk/include \
  SOAK_DURATION=2h SOAK_WORKERS=8 SOAK_SAMPLE_INTERVAL=10s

make test-native-lifecycle IMAGE="$IMAGE" INTERBASE_INCLUDE=/path/to/sdk/include
make test-fuzz INTERBASE_INCLUDE=/path/to/sdk/include INTERBASE_LIB=/path/to/client/lib FUZZ_TIME=10s
make bench INTERBASE_INCLUDE=/path/to/sdk/include INTERBASE_LIB=/path/to/client/lib
make bench-live IMAGE="$IMAGE" INTERBASE_INCLUDE=/path/to/sdk/include BENCH_TIME=1s
```

For the complete DSQL-cancellation acceptance run used by this repository,
execute the commands serially with the matching SDK and client library:

```sh
# Set IMAGE to an existing local InterBase image and BATS to a bats executable.
: "${IMAGE:?set IMAGE to an existing local InterBase image}"
: "${BATS:?set BATS to a bats executable}"
export IMAGE BATS
export INTERBASE_INCLUDE=/path/to/sdk/include
export INTERBASE_LIB=/path/to/client/lib
export LD_LIBRARY_PATH="$INTERBASE_LIB"
export CGO_CFLAGS="-I$INTERBASE_INCLUDE"
export CGO_LDFLAGS="-L$INTERBASE_LIB -Wl,-rpath,$INTERBASE_LIB -lgds"

make test INTERBASE_INCLUDE="$INTERBASE_INCLUDE" INTERBASE_LIB="$INTERBASE_LIB"
go test -race ./... -count=1 -timeout=300s
go test -gcflags=all=-d=checkptr=2 ./... -count=1 -timeout=180s
go vet -tags=integration ./...
make test-runner BATS="$BATS"
make test-cancellation IMAGE="$IMAGE" INTERBASE_INCLUDE="$INTERBASE_INCLUDE"
make test-faults IMAGE="$IMAGE" INTERBASE_INCLUDE="$INTERBASE_INCLUDE"
make test-native-lifecycle IMAGE="$IMAGE" INTERBASE_INCLUDE="$INTERBASE_INCLUDE"
make test-soak IMAGE="$IMAGE" INTERBASE_INCLUDE="$INTERBASE_INCLUDE" SOAK_DURATION=120s
make build INTERBASE_INCLUDE="$INTERBASE_INCLUDE" INTERBASE_LIB="$INTERBASE_LIB"
git diff --check
```

The Go commands above inherit the exported SDK include, link, and runtime
variables. If those variables are not exported, prefix each command with the
corresponding `/path/to/sdk/include` and `/path/to/client/lib` values.

The soak runner accepts positive integer durations in seconds, minutes or hours,
up to 24 hours, and 1–64 workers. It allows setup/cleanup time beyond the selected
workload duration. Workload flags are explicitly forwarded; ambient InterBase
connection variables are scrubbed by the ordinary Docker runner.

## Interpreting evidence

Fault tests use pre-operation barriers and a paused TCP response direction.
Nontransactional generator changes distinguish server execution from mere
connection setup and expose duplicate execution; committed table contents
distinguish commit from rollback. A subprocess bounds the deliberate blackhole
without pretending context can cancel the native call.

The soak reports actual physical churn separately from pool borrow/return
operations, verifies persisted per-worker counters, and samples Go heap,
process RSS, goroutines, file descriptors and pool counters. RSS includes Go,
native allocations and mapped pages; it is not an exact native leak counter.
Server-wide attachment counts are not currently collected. A short successful
run is not evidence of multi-hour stability or server power-loss durability.

## Concurrent lifecycle hardening

The process-wide client gate is quiescent rather than a conventional
reader/writer lock. Ordinary database, event, and Services calls are admitted
concurrently. Attachment and service/event lifecycle calls wait for native
calls already in progress, then run exclusively. A lifecycle waiter does not
close the admission window while it waits; another ordinary call can therefore
enter to release a database row lock that an existing call is waiting on.

The gate is shared by all Go packages in this process. It covers attachment
open/close/drop, event attach/cancel/detach, service attach/detach, and the
ordinary native operation entry points. It does not make the vendor client
thread-safe for arbitrary native calls made outside this driver.

Historical evidence with the InterBase `LI-V15.1.0.42` client on
2026-09-17 (before cancellation-aware admission and callback participation):

* The ungated native mixed attachment/DSQL reproducer terminated with
  `rc=134` and the observed glibc mutex assertion.
* The deterministic row-lock progress integration test passed: a waiting
  lifecycle close did not block the transaction that released the conflicting
  row lock.
* The opt-in mixed lifecycle integration test passed with four workers and
  1,000 lifecycle iterations.
* The 120-second soak passed with four workers and 109,237 operations,
  including 27,311 physical attachment replacements and no reported errors.

These are repeatable run results, not a guarantee for native calls made by
other libraries or for an unbounded stuck network operation. Keep process
supervision and the documented uncertain-result/reconciliation rules in place.

### Final verification — September 18, 2026

The complete Task 7 sequence ran serially with the Linux/amd64 InterBase 15.1
client and a configured SDK, client runtime, local image, and Bats executable:

- `make test` passed in 19.99 seconds with eight Go packages and eleven native
  ASan/leak harnesses, including native cancellation. The configured runtime
  path was supplied through
  `LD_LIBRARY_PATH`.
- Go race passed in 28.90 seconds; checkptr passed in 3.38 seconds; tagged
  integration vet passed in 1.34 seconds. These Go commands used explicit
  temporary SDK `CGO_CFLAGS`/`CGO_LDFLAGS` and `LD_LIBRARY_PATH`.
- The Bats runner passed **42/42** in 11.09 seconds.
- The DSQL cancellation target passed ten cancellation-race iterations in
  12.88 seconds (16.68 seconds including Docker setup/cleanup). It exercised
  direct and prepared execution, query/fetch cancellation, implicit and
  explicit writes, completion-winning and cancellation-winning races, and
  statement/pool recovery. The test's 30-second result limit is an external
  bound, not a cancellation-latency guarantee.
- The owned fault matrix passed in 16.72 seconds.
- The native lifecycle target passed in 6.80 seconds, including a 3.08-second
  `TestNativeLifecycleRace`.
- The four-worker, 120-second soak passed in 124.22 seconds wall-clock. The
  workload ran for 2m0.003813076s and completed 122,283 validated operations:
  61,143 committed writes, 61,140 rolled-back writes, and 61,143 persisted
  values. It performed 30,573 physical opens and closes with 30,572 identity
  replacements; throughput was 1,018.99 operations/s, average latency was
  3.925199 ms, and maximum latency was 27.478933 ms. After close, both pools
  had zero open/in-use/idle connections; the process had six file descriptors
  and two goroutines. RSS was 32,370,688 bytes after close, recorded as a
  measurement rather than a claim of native leak-freedom. The raw log was
  retained only in the verification environment.
- `make build` passed in 0.37 seconds. The final `git diff --check` is run
  after the documentation commit preparation.

The first unqualified `make test`, race, and vet attempts failed before the
complete check could pass because the client library/include paths were not
present in the process environment. They are retained as failures in the Task
7 report;
the reruns above used the explicit paths documented in the command block and
passed. No claim is made here for a multi-hour soak or for a universal native
client latency/thread-safety guarantee.

Ten bounded fuzz targets and local/live benchmarks were also executed during
this hardening work. Multi-hour soak support is implemented, but the final
measured soak was two minutes. Non-DSQL native calls remain non-interruptible;
DSQL cancellation is best effort, lifecycle admission can wait for a
long-running call, and sustained ordinary traffic can delay lifecycle work. No
server power-loss durability or additional platform/client-version guarantee
is implied.

Deferred feature requests are tracked in [TODO.md](TODO.md). The vendor TLS
hostname-verification defect is outside this hardening scope.
