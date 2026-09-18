# `events`

The `events` package exposes explicitly owned InterBase database event
subscriptions. `Subscribe` opens a separate native database attachment; it
does not borrow a `database/sql` connection or a direct `interbase.Attachment`.

```go
subscription, err := events.Subscribe(ctx, interbase.Config{
	Database: os.Getenv("INTERBASE_DATABASE"),
	User:     os.Getenv("INTERBASE_USER"),
	Password: os.Getenv("INTERBASE_PASSWORD"),
	Dialect:  3,
}, "order_created")
if err != nil {
	return err
}
defer subscription.Close()

counts, err := subscription.Next(ctx)
if err != nil {
	return err
}
if counts["order_created"] != 0 {
	// Handle one or more occurrences.
}
```

`Next` returns a map containing every subscribed name, including zero counts.
Counts are accumulated until returned, and concurrent `Next` calls are
serialized. Canceling a `Next` wait does not close the subscription or discard
already pending counts. `Close` is idempotent, may run concurrently with
`Next`, cancels native requests, waits for callbacks, and detaches the owned
attachment.

The event callback participates in the process-wide native gate through a
small C-to-Go bridge. Callback counter decoding and callback-driven re-arm use
ordinary admission; `Close` first marks the subscription closed, unregisters
future callbacks, and drains callbacks and in-progress queues without holding
exclusive admission. Each native cancel and the final detach then take the
exclusive gate only for that SDK call. A failed cancel or detach retains the
subscription's native ownership so the existing `Close` retry path can repeat
the operation. Callbacks that arrive after unregistering are ignored and do
not queue a new request. `Subscribe` and `Next` use context-aware gate
admission; the initial open does not hold an ordinary slot across failure
cleanup, because cleanup may need the exclusive cancel/detach phase.

Event timing and grouping are controlled by the InterBase server and native
client. Notifications contain accumulated counts rather than row identity or
payloads, and the API does not promise exactly-once delivery or a universal
transaction-timing contract. The disposable InterBase integration fixture
verifies that an insert is silent before commit and after rollback, and emits
one occurrence for one committed trigger execution. Query committed state when
durable row details are required. Event names are validated before attachment
and may span multiple native event blocks internally.

The package requires the same official InterBase SDK and Linux/amd64 cgo
environment as the root package. Its deterministic callback harness is used by
the Go race tests and the standalone native ASan/leak test; live coverage uses
only the disposable InterBase Docker fixture described in
[`integration/README.md`](../integration/README.md).

When `interbase.Config.TLS` is enabled, this package passes the native TLS
attachment options through to its separate event attachment. With vendor client
`LI-V15.1.0.42`, trusted CA material does not prove hostname verification: the
strict TLS fixture reproduces wrong-hostname acceptance. See the TLS security
warning in [`integration/README.md`](../integration/README.md#isolated-native-tls-security-contracts);
there is intentionally no Go-side preflight workaround.
