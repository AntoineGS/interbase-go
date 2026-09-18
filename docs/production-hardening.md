# Production hardening

## Execution contract

`Config.ConnectTimeout` sets the native attachment connection timeout. Zero
keeps the client default; positive durations round upward to whole seconds.
Negative durations and values exceeding the native unsigned 32-bit seconds
field are rejected. This is an attachment timeout, not a query deadline.

Context cancellation prevents execution when observed before native entry.
It cannot interrupt an in-flight native call. A successfully completed
`database/sql` execution returns its affected-row result even if cancellation
arrives during that call; returning a result together with an error would make
`database/sql` discard the result. A native error is preserved rather than
converted to `driver.ErrBadConn` after a potentially executed operation.

An error after a lost response does not prove a write failed. Applications must
reconcile uncertain results using their own operation identifiers or database
state before retrying. Explicit transaction execution success does not imply
that its later commit succeeded.

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
make test-faults IMAGE=private-interbase:local INTERBASE_INCLUDE=/path/to/sdk/include

# Concurrent work, commit/rollback correctness, early rows.Close, physical churn.
make test-soak IMAGE=private-interbase:local INTERBASE_INCLUDE=/path/to/sdk/include \
  SOAK_DURATION=120s SOAK_WORKERS=4 SOAK_SAMPLE_INTERVAL=1s

# Longer runs use the same workload; duration is not a claim of prior verification.
make test-soak IMAGE=private-interbase:local INTERBASE_INCLUDE=/path/to/sdk/include \
  SOAK_DURATION=2h SOAK_WORKERS=8 SOAK_SAMPLE_INTERVAL=10s

make test-native-lifecycle IMAGE=private-interbase:local INTERBASE_INCLUDE=/path/to/sdk/include
make test-fuzz INTERBASE_INCLUDE=/path/to/sdk/include INTERBASE_LIB=/path/to/client/lib FUZZ_TIME=10s
make bench INTERBASE_INCLUDE=/path/to/sdk/include INTERBASE_LIB=/path/to/client/lib
make bench-live IMAGE=private-interbase:local INTERBASE_INCLUDE=/path/to/sdk/include BENCH_TIME=1s
```

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

Cancellation-aware admission now returns before entering native code when its
context expires while waiting. Mandatory cleanup remains uninterruptible.
Events callbacks participate in the same gate through the C/Go bridge; stop
unregisters and drains callback/queue work before exclusive native cancellation.
Canceled Events `Next` calls preserve the subscription and context error identity.
Independent review approved the corrected admission and callback sequencing.

Fresh controller checks passed:

- All eight Go packages and ten native ASan/leak harnesses.
- Full Go race checks, checkptr, and tagged integration vet.
- Full ordinary Docker integration suite and the owned fault matrix.
- Lifecycle churn and both row-lock commit/rollback progress regressions,
  repeated three times with the race detector.
- All 36 runner tests. One initial parallel lifecycle runner failed to start
  its container; the subsequent serial run completed all requested tests.

The final four-worker, 120-second soak completed 95,389 validated operations:
47,696 committed writes, 47,693 rolled-back writes, and exactly 47,696 persisted
counter increments. It performed 23,850 physical opens and closes, with 23,849
observed replacement identities. Throughput was 794.87 workload iterations/s;
average iteration latency was 5.032 ms. Both pools were empty after close;
file descriptors fell from 7 before the workload to 6 afterward, and goroutines
from 4 to 2. Process RSS grew from about 16.5 MB to 26.9 MB, which is recorded
as a measurement rather than a claim of native leak-freedom. The raw final soak
log is `/tmp/opencode/hardening-final-soak.log` in the verification environment.

Ten bounded fuzz targets and local/live benchmarks were also executed during
this hardening work. Multi-hour soak support is implemented, but the final
measured soak was two minutes. In-flight native calls remain non-interruptible;
lifecycle admission can wait for a long-running call, and sustained ordinary
traffic can delay lifecycle work. No server power-loss durability or additional
platform/client-version guarantee is implied.

Deferred feature requests are tracked in [TODO.md](TODO.md). The vendor TLS
hostname-verification defect is outside this hardening scope.
