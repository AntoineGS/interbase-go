# Task 3 Report: Prepared Statement and Cursor Fetch Cancellation

## Result

Task 3 is implemented on top of `bad888d` and committed as:

```text
Cancel prepared execution and fetch
```

The implementation covers prepared statement execution, prepared queries,
`database/sql` row fetching, and direct cursor fetching. Transient
prepare/query/execute cancellation remains deferred to Task 4 as required.

## Implementation

### Native cancellation publication

- Extended the prepared execution, prepared query, and cursor-fetch C entry
  points with an `ib_cancel_slot` and generation.
- Published the actual `isc_stmt_handle` immediately before
  `isc_dsql_execute`, `isc_dsql_execute2`, or `isc_dsql_fetch`.
- Completed the cancellation slot immediately after each native call and on
  every pre-call failure path.
- Kept contextless internal/catalog callers on an idle (`NULL`, generation
  zero) slot path.
- Ensured slot completion precedes transaction rollback, cursor cleanup,
  statement cleanup, and other ownership-releasing paths.

### Go propagation

- Changed `nativeStatement.exec`, `nativeStatement.query`, and
  `nativeCursor.next` to accept `context.Context`.
- Created and finished one `nativeCancelOperation` around each cancelable
  prepared execution/query/fetch call.
- Preserved contextless `database/sql/driver` methods by routing `Exec` and
  `Query` through `context.Background()`.
- Propagated the operation context through `stmt.ExecContext`,
  `stmt.QueryContext`, `rows.Next`, and direct `Cursor.Next`.
- Preserved the known-success rule when native execution completes before a
  concurrent cancellation is observed.
- Classified cancellation using the existing native outcome classifier,
  retaining native cancellation diagnostics without converting cancellation
  to `driver.ErrBadConn`.

## Tests added or extended

- Prepared `database/sql` execution cancellation, cleanup ordering, and
  statement reuse after a canceled generation.
- Prepared `database/sql` query cancellation.
- `database/sql` row-fetch cancellation and delayed abort verification.
- Direct cursor-fetch cancellation and cleanup verification.
- Native publication and cleanup-order assertions for execute, execute2, and
  fetch, including generation and actual statement-handle checks.

## Fresh verification

All commands below were run against the current worktree using the temporary
InterBase SDK/client at `/tmp/opencode`:

```text
git diff --check                                  PASS
go test ./... -count=1 -timeout=300s               PASS
go test -race ./... -count=1 -timeout=300s         PASS
go test -gcflags=all=-d=checkptr=2 ./... ...       PASS
go vet ./...                                      PASS
go test -race . -run 'Test(DatabaseSQLPrepared|RowsNext|DirectCursorNext)' ... PASS
make test-native ...                              PASS
```

`make test-native` passed all existing native harnesses, including the
prepared, values, direct, lifecycle, distributed, services, events, and
cancellation ASan harnesses with leak detection enabled.

No live integration cancellation test was run; that coverage remains outside
this task's scope. Pre-existing untracked planning/specification artifacts in
the worktree were not staged.

## Round 1/5 review fixes

### P2: authoritative successful results

The post-success context checks in `stmt.QueryContext`, `rows.Next`, and
direct `Cursor.Next` were removing results after the native query/fetch had
already completed successfully. Those checks were removed. Context checks
before admission and before starting the native operation remain in place;
successful query, row, EOF, and direct-cursor results are now authoritative
even when cancellation is observed immediately afterward.

Added deterministic completion-vs-cancel regressions for:

- prepared `database/sql` query completion;
- `database/sql` rows fetch completion; and
- direct cursor fetch completion.

### P2: overlapping production-slot cancellation

Extended `tests/native_prepared_test.c` with pthread barriers that exercise the
actual cancellation-slot path around `isc_dsql_execute`, `isc_dsql_execute2`,
and `isc_dsql_fetch`. Each test delays `DSQL_cancel`, releases the native call,
observes slot completion waiting on the live cancel user, and verifies that
rollback/close/drop cleanup has not started until the cancellation call exits.
The fetch case additionally covers a failing cancellation request, cursor
cleanup, reuse of the same prepared statement, and a follow-up prepared
execution on the same explicit connection transaction. The prepared ASan
harness is now linked with `-pthread` by `make test-native`.

No public direct prepared-statement API was added. Direct coverage remains on
the existing transaction/cursor API (`Cursor.Next`); this API-scope ruling is
recorded here for the task ledger.

## Round 1 exact verification evidence

All commands below were run after the fixes with the SDK/client at
`/tmp/opencode` and `INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include`.

### TDD focused cycle

RED command:

```sh
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test . -run 'Test(DatabaseSQLPreparedQueryContextPreserves|RowsNextPreserves|DirectCursorNextPreserves)' \
  -count=1 -timeout=120s
```

Observed exit status: `1`. The three new tests failed at their expected
post-cancellation success assertions: prepared query returned
`context canceled`, `rows.Next` returned `context canceled`, and direct
`Cursor.Next` returned `context canceled`.

GREEN command:

```sh
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test . -run 'Test(DatabaseSQLPrepared(QueryContextPreserves|ExecContextCancels|ExecContextReuses)|RowsNext(Preserves|Cancels)|DirectCursorNext(Preserves|Cancels))' \
  -count=1 -timeout=120s
```

Observed output: `ok interbase-go 0.065s`.

Repeated focused race command:

```sh
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test -race . -run 'Test(DatabaseSQLPreparedQueryContextPreserves|RowsNextPreserves|DirectCursorNextPreserves)' \
  -count=20 -timeout=180s
```

Observed output: `ok interbase-go 1.038s`.

### Native production-slot barriers

Non-ASan focused harness:

```sh
cc -std=c11 -Wall -Wextra -Werror -g -O0 \
  -I/tmp/opencode/interbase-parity-include tests/native_prepared_test.c \
  -L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds -pthread \
  -o /tmp/opencode/native_prepared_round1
timeout 30s /tmp/opencode/native_prepared_round1
```

Observed output: `native prepared tests passed`.

Prepared ASan harness:

```sh
cc -std=c11 -Wall -Wextra -Werror -g -O1 -fsanitize=address \
  -fno-omit-frame-pointer -I/tmp/opencode/interbase-parity-include \
  tests/native_prepared_test.c \
  -L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds -pthread \
  -o /tmp/opencode/native_prepared_round1_asan
ASAN_OPTIONS=detect_leaks=1 /tmp/opencode/native_prepared_round1_asan
```

Observed output: `native prepared tests passed`; no sanitizer or leak
diagnostics were emitted.

### Complete relevant suites

The following all exited `0`:

```sh
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test ./... -count=1 -timeout=300s

CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test -race ./... -count=1 -timeout=300s

CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test -gcflags=all=-d=checkptr=2 ./... -count=1 -timeout=300s

CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go vet ./...

make test-native INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
  INTERBASE_LIB=/tmp/opencode

git diff --check
```

The Go suites reported `ok` for all packages (`interbase-go`, `cmd/ibprobe`,
`events`, `internal/faultproxy`, `internal/nativegate`,
`internal/testfixture`, `schema`, and `services`). `go vet` produced no
diagnostics. `make test-native` passed the status, values, direct, lifecycle,
plan, BLOB, prepared, distributed, services, events, and cancellation ASan
harnesses with leak detection enabled.
