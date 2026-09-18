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
