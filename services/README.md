# InterBase Services Manager

The `services` package exposes a bounded, typed subset of the InterBase
Services Manager through the official native client library. It uses cgo and
does not implement the Services wire protocol or accept arbitrary parameter
blocks.

## Requirements

- Linux/amd64, Go 1.23+, and `CGO_ENABLED=1`.
- The official InterBase SDK headers and matching `libgds.so` client library.
- A Services Manager account for live operations, normally a dedicated
  administrative account.

The default cgo paths are `/opt/interbase/include` and
`/opt/interbase/lib`. Repository Make targets accept
`INTERBASE_INCLUDE=/path/to/include INTERBASE_LIB=/path/to/lib` overrides.
For direct Go commands, supply matching `CGO_CFLAGS`, `CGO_LDFLAGS`, and, when
needed, `LD_LIBRARY_PATH`. These overrides do not establish support for other
platforms.

## Opening a manager

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

manager, err := services.Open(ctx, services.Config{
    Host:     "localhost",
    User:     "SYSDBA",
    Password: os.Getenv("INTERBASE_PASSWORD"),
})
if err != nil {
    // In an opening helper returning (*services.Manager, error), preserve the
    // owner for the caller if cleanup fails. Never discard a live Manager.
    if manager != nil {
        if closeErr := manager.Close(); closeErr != nil {
            return manager, errors.Join(err, closeErr)
        }
    }
    return nil, err
}
return manager, nil // The caller owns Close, including retries on failure.
```

An empty `Host` targets the local `service_mgr`; a non-empty host receives the
`:service_mgr` suffix when it is absent. TLS attachment options are carried by
`Config.TLS` and are validated before the native client is called. `Open` may
return both a non-nil quarantined `Manager` and an error after a failed native
attach; callers must close that manager. Each `Close` attempt is shared by
concurrent callers. A failed attempt retains native ownership for a later
explicit `Close` retry, and no new operation is accepted while cleanup remains
unresolved.

## Operations

The package provides typed requests for:

- server information, capabilities, connection/database listings, and log
  retrieval;
- logical backup, restore, dump, archive, and tablespace operations;
- database statistics, validation, sweep, limbo listing/resolution, and
  database property changes;
- security-database users and database aliases.

Asynchronous operations return `*Job`, which implements `io.Reader` for bounded
output chunks:

```go
job, err := manager.DatabaseStatistics(ctx, services.DatabaseStatisticsRequest{
    Database: "/srv/interbase/data/example.ib",
})
if err != nil {
    return err
}
if err := job.Wait(ctx); err != nil {
    return err
}
```

`Job.Wait` drains unread output. `Job.Close` discards unread output, drains the
native operation, and releases the manager for another action. A manager allows
one active operation at a time; context cancellation does not claim to cancel
an already-running native call. Always close the manager and jobs, including on
error paths. Concurrent `Manager.Close` calls share one detach attempt. A
failed detach keeps the native backend owned by the manager for an explicit
retry, while new operations remain rejected.

Synchronous property, user, alias, and limbo-resolution methods fully drain
their native service action before returning. Server-side paths are interpreted
by the server, not by the client process.

## Recovering a prepared distributed transaction

`DistributedTransaction.RecoveryInfo` returns a copied, serializable entry for
each participant in the same order supplied to `BeginDistributed`. Persist the
result before attempting an operation whose native completion can be
ambiguous. Each entry contains the opaque database identity returned by
`Attachment.DatabaseInfo(InfoDatabaseID)` and the participant's numeric
transaction identity from `Transaction.Info(InfoTransactionID)`.

After a prepared transaction is handed to an external recovery coordinator,
record one commit-or-rollback decision using the application's durability
protocol, then resolve every participant against its own database path:

```go

// Keep the database paths alongside the participant list supplied to
// BeginDistributed; both lists use that same stable order.
for index, participant := range recoveryInfo {
    transactionID, err := participant.TransactionID.Uint64()
    if err != nil {
        return err
    }
    request := services.ResolveLimboRequest{
        Database:      databasePaths[index],
        TransactionID: int64(transactionID),
    }
    if commit {
        if err := manager.CommitLimbo(ctx, request); err != nil {
            return err
        }
    } else if err := manager.RollbackLimbo(ctx, request); err != nil {
        return err
    }
}
```

`ReleaseAfterRecovery` and `Abandon` only release local Go/native ownership;
they never commit or roll back the server transaction. Use them only after the
recovery record and external decision are durable, and do not call the
distributed coordinator again after either method succeeds. `ListLimbo` returns
an asynchronous `Job`; read or wait for it to completion before starting a
resolution on the same Services Manager.

If an ambiguous native completion consumes the local coordinator handle,
subsequent `Commit` and `Rollback` calls return
`ErrDistributedNativeUnavailable`. Use the copied `RecoveryInfo` with the
Services Manager to resolve limbo, then release the local coordinator with
`ReleaseAfterRecovery` or `Abandon`.

The live client-death test writes its private recovery record before `Prepare`
with a write, file sync, and close. The parent process—not the child—waits for
the exact `ListLimbo` matches, writes the `commit\n` or `rollback\n` decision,
syncs and closes that file, and only then calls the Services resolution methods.
Those writes are not a power-loss durability proof: the test does not restart
the server or simulate power loss. Applications claiming that guarantee also
need an appropriate atomic replacement and directory-sync protocol.

`RestoreOptions.NoDatabaseTriggers` is explicitly rejected with `ErrUnsupported`
because the supplied official SDK header does not expose the corresponding
native option. The implementation intentionally does not claim full Python
Services API parity; unsupported or unimplemented operations must be added as
typed requests with matching native and live coverage.

## Testing

Offline package tests use a small injected backend for manager/job lifecycle and
real request/decoder code for protocol contracts. Live service tests use only
owned temporary databases in the disposable Docker runner:

```sh
INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include \
CGO_CFLAGS='-I/tmp/opencode/interbase-parity-include' \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode' \
IMAGE='interbase-go-parity-test:local' \
./scripts/test-integration-docker.sh
```

Do not point the live suite at an existing application database. Native calls
are not interruptible in flight, so use a process-level timeout for stalled
experiments.
