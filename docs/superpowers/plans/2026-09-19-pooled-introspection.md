# Pooled Introspection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reach `Diagnostics` (database SQL dialect and version facts) and prepare-only `Plan` through a connection borrowed from a pooled `*sql.DB`, without opening a second native attachment.

**Architecture:** The unexported `*conn` that `database/sql` already owns gains two exported methods, `Diagnostics` and `Plan`. `Attachment.Diagnostics` is refactored to delegate to the shared `(*conn).Diagnostics` so the direct and pooled paths cannot drift. Package-level `Diagnostics(ctx, *sql.Conn)` and `Plan(ctx, *sql.Conn, query)` helpers wrap `(*sql.Conn).Raw`, assert the callback value to a new exported `Introspector` interface, copy the result out, and return the driver error unchanged. `Plan` calls `c.native.prepare` and never executes: native `ib_statement_prepare_mode` selects the connection's active explicit transaction when `BeginTx` started one and otherwise starts and completes its own read-only transaction inside the prepare call, so Go makes no transaction decision.

**Tech Stack:** Go 1.23, `database/sql`, `database/sql/driver`, cgo, InterBase 15.1 native client, Docker integration fixtures, ASan.

**Spec:** `docs/superpowers/specs/2026-09-19-pooled-introspection-design.md`

## Global Constraints

Values copied verbatim from the spec. Every task's requirements implicitly include this section.

- Baseline is commit `f19d654` on `main`. (At planning time `main` is `d859a5c`; every commit between the two is documentation-only, so the code citations below still hold.)
- Supported platform scope is unchanged: **Linux/amd64 against the tested InterBase 15.1 client/server stack.**
- **No new dependency is needed.** `interbase.go` already imports `database/sql/driver` and `database/sql`.
- **`Plan` prepares and never executes, in any mode.** No `isc_dsql_execute`, `isc_dsql_execute2`, or `isc_dsql_fetch` is reachable from this path, for any statement type.
- `Plan` uses `validateDatabaseSQLQuery` — the pooled validator — so `SET SUBSCRIPTION` is redirected to the direct API exactly as `PrepareContext` does, rather than the direct-only `validateQueryText`.
- The helpers never rewrite an error. **In particular they never manufacture `driver.ErrBadConn`, and never downgrade a classified cancellation error to it.**
- The `Introspector` value is valid only inside the `Raw` callback. The helpers do not store it, and the documentation states that callers must not either.
- A nil `*sql.Conn` returns `ErrNotInterBaseConn` rather than panicking.
- `enterNativeContext` admission is acquired and released on every path, including the early-error paths.
- **Cancellation adds nothing new.** `Plan` inherits the existing best-effort DSQL cancellation contract and adds nothing to it. No new cancellation guarantees, no latency claim, no thread-safety claim, no plan-format claim.
- `Attachment.Diagnostics` keeps returning the same values through the extracted implementation. No existing signature, error, or behavior changes.
- Excluded, and to stay excluded: any `DatabaseInfo`/`InfoItem` passthrough on the pooled path; a pooled equivalent of `Transaction.Info`; array binding or array-producing plans through `Raw`; BLOB streaming, cursor naming, ChangeView, or any other direct-only surface through `Raw`.
- **`make test` must stay green without a server: every new test that needs one is behind the `integration` tag.**
- All new live tests carry `//go:build integration` and use the existing fixture helpers, so they are covered by `make test-integration-docker`.
- `openDatabase` sets `SetMaxOpenConns(1)`, so any test that needs two concurrent attachments must call `createFixture` once and `openDatabase` twice against the same fixture path rather than taking two `*sql.Conn` from one pool.
- Use TDD for every behavior change: observe the intended test fail before production implementation.
- Do not publish the private test image, SDK, client library, fixture credentials, or generated database files.

### Build and test commands

Unit tests need no server. If the official client lives outside `/opt/interbase`, pass the overrides:

```sh
make test INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include INTERBASE_LIB=/tmp/opencode
```

For a single focused Go test run, the equivalent direct invocation is:

```sh
CGO_CFLAGS=-I/tmp/opencode/interbase-parity-include \
CGO_LDFLAGS='-L/tmp/opencode -Wl,-rpath,/tmp/opencode -lgds' \
LD_LIBRARY_PATH=/tmp/opencode \
go test . -run '^TestName$' -count=1
```

Every `go test` command in this plan assumes that environment prefix. Where the plan writes `go test . -run ...`, use the prefixed form if `/opt/interbase` is not populated.

---

## File Structure

- **Create `introspection.go`** — the whole pooled introspection surface: the `Introspector` interface, `ErrNotInterBaseConn`, the package-level `Diagnostics`/`Plan` helpers, and the shared `(*conn).Diagnostics`/`(*conn).Plan` methods. One file, one responsibility: "reach direct-API introspection through a pooled connection". Keeping `(*conn).Diagnostics` here rather than in the already 1840-line `interbase.go` or the 2201-line `direct.go` keeps the shared implementation next to its only two callers.
- **Create `introspection_test.go`** — unit tests for everything in `introspection.go`, plus the fake-native helpers they share.
- **Modify `direct.go`** — `Attachment.Diagnostics` (`direct.go:440`) loses its body and delegates to `(*conn).Diagnostics`.
- **Modify `native.go`** — add the `planOverride` test seam to `nativeStatement` (`native.go:764-772`) and honor it in `(*nativeStatement).plan` (`native.go:1464`).
- **Modify `connect_timeout_test.go`** — add `databaseSQLTestFactoryConnector`, a sibling of the existing `databaseSQLTestConnector` (`connect_timeout_test.go:13-23`) that mints a fresh `driver.Conn` per `Connect` call.
- **Create `integration/introspection_test.go`** — the live tests, behind `//go:build integration`.
- **Modify `README.md`** — "Go Usage" example, "Supported Boundary" entry, "Known Limits" entry.

---

### Task 1: Shared `(*conn).Diagnostics` Behind `Attachment.Diagnostics`

Move the diagnostics decoding out of `Attachment.Diagnostics` so the pooled path can reuse it verbatim. Nothing public changes in this task.

**Files:**
- Create: `introspection.go`
- Create: `introspection_test.go`
- Modify: `direct.go:440-539` (`Attachment.Diagnostics`)

**Interfaces:**
- Consumes: `contextError(ctx context.Context) error` (`interbase.go:1554`), `(*conn).lockDirect()` (`interbase.go:740`), `(*conn).sanitizeError(operation string, err error) error` (`interbase.go:733`), `enterNativeContext(ctx context.Context) (func(), error)` (`native.go:144`), `(*nativeConnection).databaseInfo(item byte) ([]byte, error)` (`native.go:1718`), `(*nativeConnection).clientVersion() (string, error)` (`native.go:1116`), `parseInfoItem(response []byte, requested byte) (InfoItem, error)` (`direct.go:1966`), `DatabaseDiagnostics` (`direct.go:125`), `errDirectAttachmentClosed` (`direct.go:54`).
- Produces: `func (c *conn) Diagnostics(ctx context.Context) (DatabaseDiagnostics, error)`.
- Produces test helpers used by Tasks 4–7: `func introspectionInfoPayload(code byte, data []byte) []byte`, `func introspectionInfoResponder(dialect byte) func(byte) ([]byte, error)`, `func introspectionTestNative(dialect byte) *nativeConnection`, `func introspectionWantDiagnostics(dialect int64) DatabaseDiagnostics`.

- [ ] **Step 1: Write the failing test**

Create `introspection_test.go`:

```go
package interbase

import (
	"context"
	"errors"
	"testing"
)

func introspectionInfoPayload(code byte, data []byte) []byte {
	result := make([]byte, 0, len(data)+4)
	result = append(result, code, byte(len(data)), byte(len(data)>>8))
	result = append(result, data...)
	return append(result, infoEnd)
}

// introspectionInfoResponder reproduces the native information framing used by
// TestAttachmentDiagnosticsDecodesNativeInfoAndClientVersion
// (direct_lifecycle_test.go:869), with a selectable SQL dialect payload.
func introspectionInfoResponder(dialect byte) func(byte) ([]byte, error) {
	return func(code byte) ([]byte, error) {
		switch code {
		case InfoDatabaseVersion:
			return introspectionInfoPayload(code, []byte{1, 3, 'V', '1', '0'}), nil
		case InfoDatabaseODSVersion:
			return introspectionInfoPayload(code, []byte{13, 0}), nil
		case InfoDatabaseODSMinorVersion:
			return introspectionInfoPayload(code, []byte{1, 0}), nil
		case InfoDatabasePageSize:
			return introspectionInfoPayload(code, []byte{0, 16, 0, 0}), nil
		case InfoDatabaseSQLDialect:
			return introspectionInfoPayload(code, []byte{dialect}), nil
		case InfoDatabaseReadOnly:
			return introspectionInfoPayload(code, []byte{0}), nil
		default:
			return nil, errors.New("unexpected info code")
		}
	}
}

func introspectionTestNative(dialect byte) *nativeConnection {
	return &nativeConnection{
		brokenOverride:        func() bool { return false },
		databaseInfoOverride:  introspectionInfoResponder(dialect),
		clientVersionOverride: func() (string, error) { return "client-v1", nil },
		closeOverride:         func() error { return nil },
	}
}

func introspectionWantDiagnostics(dialect int64) DatabaseDiagnostics {
	return DatabaseDiagnostics{
		ClientVersion:   "client-v1",
		ServerVersion:   "V10",
		DatabaseVersion: "13.1",
		ODSVersion:      13,
		ODSMinorVersion: 1,
		PageSize:        4096,
		SQLDialect:      dialect,
		ReadOnly:        false,
	}
}

func TestConnDiagnosticsDecodesEveryFieldForEachDialect(t *testing.T) {
	for _, dialect := range []byte{1, 3} {
		connection := &conn{native: introspectionTestNative(dialect)}
		got, err := connection.Diagnostics(context.Background())
		if err != nil {
			t.Fatalf("dialect %d: (*conn).Diagnostics() error = %v", dialect, err)
		}
		want := introspectionWantDiagnostics(int64(dialect))
		if got != want {
			t.Fatalf("dialect %d: (*conn).Diagnostics() = %#v, want %#v", dialect, got, want)
		}
	}
}

func TestAttachmentDiagnosticsAndConnDiagnosticsAgree(t *testing.T) {
	attachmentConn := &conn{native: introspectionTestNative(3)}
	attachment := &Attachment{conn: attachmentConn}
	fromAttachment, err := attachment.Diagnostics(context.Background())
	if err != nil {
		t.Fatalf("Attachment.Diagnostics() error = %v", err)
	}
	pooledConn := &conn{native: introspectionTestNative(3)}
	fromConn, err := pooledConn.Diagnostics(context.Background())
	if err != nil {
		t.Fatalf("(*conn).Diagnostics() error = %v", err)
	}
	if fromAttachment != fromConn {
		t.Fatalf("Attachment.Diagnostics() = %#v, (*conn).Diagnostics() = %#v; the shared implementation drifted",
			fromAttachment, fromConn)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test . -run 'TestConnDiagnostics|TestAttachmentDiagnosticsAndConnDiagnosticsAgree' -count=1`

Expected: FAIL — compilation error, `connection.Diagnostics undefined (type *conn has no field or method Diagnostics)`.

- [ ] **Step 3: Create `introspection.go` with the moved implementation**

The body below is `Attachment.Diagnostics` (`direct.go:440-539`) with `a.conn` rewritten to `c`, and with the nil-receiver check left behind on `Attachment`. Nothing else changes: no `invalidateLocked` is added, because the original has none and this task must not alter `Attachment.Diagnostics` behavior.

```go
package interbase

import (
	"context"
	"database/sql/driver"
	"fmt"
)

// Diagnostics returns server, database, and linked client diagnostics for the
// attachment behind this connection. It is the shared implementation used by
// both Attachment.Diagnostics and the pooled Diagnostics helper.
func (c *conn) Diagnostics(ctx context.Context) (DatabaseDiagnostics, error) {
	if err := contextError(ctx); err != nil {
		return DatabaseDiagnostics{}, err
	}
	c.lockDirect()
	defer c.mu.Unlock()
	if c.closed || c.native == nil || c.native.broken() {
		return DatabaseDiagnostics{}, driver.ErrBadConn
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	defer releaseNative()
	readInfo := func(code byte) (InfoItem, error) {
		if err := contextError(ctx); err != nil {
			return InfoItem{}, err
		}
		response, err := c.native.databaseInfo(code)
		if err != nil {
			return InfoItem{}, c.sanitizeError("database diagnostics", err)
		}
		item, err := parseInfoItem(response, code)
		if err != nil {
			return InfoItem{}, err
		}
		if err := contextError(ctx); err != nil {
			return InfoItem{}, err
		}
		return item, nil
	}
	serverVersionItem, err := readInfo(InfoDatabaseVersion)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	serverVersion, err := serverVersionItem.Text()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsItem, err := readInfo(InfoDatabaseODSVersion)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsVersion, err := odsItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsMinorItem, err := readInfo(InfoDatabaseODSMinorVersion)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsMinorVersion, err := odsMinorItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	pageSizeItem, err := readInfo(InfoDatabasePageSize)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	pageSize, err := pageSizeItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	dialectItem, err := readInfo(InfoDatabaseSQLDialect)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	dialect, err := dialectItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	readOnlyItem, err := readInfo(InfoDatabaseReadOnly)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	readOnly, err := readOnlyItem.Bool()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	if err := contextError(ctx); err != nil {
		return DatabaseDiagnostics{}, err
	}
	clientVersion, err := c.native.clientVersion()
	if err != nil {
		return DatabaseDiagnostics{}, c.sanitizeError("client diagnostics", err)
	}
	return DatabaseDiagnostics{
		ClientVersion:   clientVersion,
		ServerVersion:   serverVersion,
		DatabaseVersion: fmt.Sprintf("%d.%d", odsVersion, odsMinorVersion),
		ODSVersion:      odsVersion,
		ODSMinorVersion: odsMinorVersion,
		PageSize:        pageSize,
		SQLDialect:      dialect,
		ReadOnly:        readOnly,
	}, nil
}
```

- [ ] **Step 4: Replace the `Attachment.Diagnostics` body with delegation**

In `direct.go`, replace the whole function starting at `direct.go:440` (from `func (a *Attachment) Diagnostics` through its closing brace at `direct.go:539`) with:

```go
// Diagnostics returns server, database, and linked client diagnostics for the
// attachment.
func (a *Attachment) Diagnostics(ctx context.Context) (DatabaseDiagnostics, error) {
	if err := contextError(ctx); err != nil {
		return DatabaseDiagnostics{}, err
	}
	if a == nil || a.conn == nil {
		return DatabaseDiagnostics{}, errDirectAttachmentClosed
	}
	return a.conn.Diagnostics(ctx)
}
```

Then check whether `direct.go` still uses `fmt`: it does (many other call sites), so leave its import list alone. If `go build` reports an unused import in `direct.go`, remove only the reported one.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test . -run 'TestConnDiagnostics|TestAttachmentDiagnostics' -count=1`

Expected: PASS, including the pre-existing `TestAttachmentDiagnosticsDecodesNativeInfoAndClientVersion` (`direct_lifecycle_test.go:869`), which must still pass unchanged — it is the proof that the move was behavior-preserving.

- [ ] **Step 6: Run the full unit suite**

Run: `go test . -count=1 -timeout=60s`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add introspection.go introspection_test.go direct.go
git commit -m "Share the diagnostics implementation with the pooled connection"
```

---

### Task 2: `planOverride` Seam on `nativeStatement`

`(*nativeStatement).plan` (`native.go:1464`) hard-fails with `"native statement is unavailable"` when `s.ptr == nil`, so a fake statement built by a `prepareOverride` can never be planned. Add the override field, matching the shape of the existing seams at `native.go:764-772`.

**Files:**
- Modify: `native.go:764-772` (`nativeStatement` struct)
- Modify: `native.go:1464-1484` (`(*nativeStatement).plan`)
- Modify: `introspection_test.go`

**Interfaces:**
- Consumes: `nativegate.Global.Enter() func()` (`internal/nativegate/gate.go:90`).
- Produces: field `planOverride func() (string, error)` on `nativeStatement`, honored by `func (s *nativeStatement) plan() (string, error)`.

- [ ] **Step 1: Write the failing test**

Append to `introspection_test.go`:

```go
func TestNativeStatementPlanUsesOverrideWithoutNativePointer(t *testing.T) {
	statement := &nativeStatement{
		planOverride: func() (string, error) { return "PLAN (GO_COUNTRY INDEX (PK_GO_COUNTRY))", nil },
	}
	plan, err := statement.plan()
	if err != nil {
		t.Fatalf("(*nativeStatement).plan() error = %v", err)
	}
	if plan != "PLAN (GO_COUNTRY INDEX (PK_GO_COUNTRY))" {
		t.Fatalf("(*nativeStatement).plan() = %q, want the override value", plan)
	}
}

func TestNativeStatementPlanReturnsOverrideError(t *testing.T) {
	planErr := errors.New("injected plan failure")
	statement := &nativeStatement{
		planOverride: func() (string, error) { return "", planErr },
	}
	if _, err := statement.plan(); !errors.Is(err, planErr) {
		t.Fatalf("(*nativeStatement).plan() error = %v, want the override error", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test . -run 'TestNativeStatementPlan' -count=1`

Expected: FAIL — compilation error, `unknown field planOverride in struct literal of type nativeStatement`.

- [ ] **Step 3: Add the field**

In `native.go`, replace the struct at `native.go:764-772` with:

```go
type nativeStatement struct {
	ptr                       *C.ib_statement
	execOverride              func([]argument) (int64, error)
	queryOverride             func([]argument) (*nativeCursor, []string, error)
	queryMutatingOverride     func() bool
	writeOutcomeStateOverride func() nativeWriteOutcomeState
	closeOverride             func() error
	numInputOverride          func() int
	planOverride              func() (string, error)
}
```

- [ ] **Step 4: Honor the field in `plan`**

In `native.go`, insert the override check immediately after the gate admission in `(*nativeStatement).plan`, before the nil-pointer guard, so the seam is reachable without a native statement:

```go
func (s *nativeStatement) plan() (string, error) {
	release := nativegate.Global.Enter()
	defer release()
	if s != nil && s.planOverride != nil {
		return s.planOverride()
	}
	if s == nil || s.ptr == nil {
		return "", errors.New("native statement is unavailable")
	}
	var planPointer *C.char
	var planLength C.size_t
	var errorPointer *C.char
	if result := C.ib_statement_plan(s.ptr, &planPointer, &planLength, &errorPointer); result != 0 {
		return "", takeNativeError(errorPointer)
	}
	if planPointer == nil {
		return "", errors.New("native statement plan is unavailable")
	}
	defer C.free(unsafe.Pointer(planPointer))
	if uint64(planLength) > uint64(math.MaxInt32) {
		return "", errors.New("native statement plan is too long")
	}
	return string(C.GoBytes(unsafe.Pointer(planPointer), C.int(planLength))), nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test . -run 'TestNativeStatementPlan' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add native.go introspection_test.go
git commit -m "Add a plan test seam to the native statement"
```

---

### Task 3: `(*conn).Plan` Prepare-Only Implementation

**Files:**
- Modify: `introspection.go`
- Modify: `introspection_test.go`

**Interfaces:**
- Consumes: `validateDatabaseSQLQuery(query string) error` (`interbase.go:594`), `ErrDistributedParticipantManaged` (`direct.go:58`), `(*nativeConnection).prepare(ctx context.Context, query string) (*nativeStatement, error)` (`native.go:1760`), `(*nativeStatement).plan() (string, error)` with the Task 2 `planOverride`, `(*nativeStatement).close() error` (`native.go:1441`), `nativeCancellationEvidenceOf(err error) nativeCancellationEvidence` (`cancellation.go:153`), `splitNativeExecutionError(err error) nativeExecutionParts` (`cancellation.go:137`), `classifyNativeOutcome(operation string, mutating bool, contextErr error, nativeErr, cleanupErr error, evidence ...nativeCancellationEvidence) error` (`cancellation.go:584`), `contextCancellation(ctx context.Context) error` (`cancellation.go:573`), `joinNativeRequestDiagnostic(operation, request error) error` (`cancellation.go:657`), `(*conn).invalidateLocked(cause error)` (`interbase.go:1352`).
- Produces: `func (c *conn) Plan(ctx context.Context, query string) (string, error)`.

The error handling below mirrors `(*conn).PrepareContext` (`interbase.go:1070-1119`) exactly, not `Transaction.Plan`: the *method* `c.sanitizeError` (which applies `c.redactionSecrets`), `classifyNativeOutcome` with `contextCancellation(ctx)` as the third argument — **not** `contextError(ctx)` — then `joinNativeRequestDiagnostic`, then `invalidateLocked` when `c.native.broken()`.

- [ ] **Step 1: Write the failing tests**

Append to `introspection_test.go`:

```go
func TestConnPlanReturnsPlanAndClosesTheStatementExactlyOnce(t *testing.T) {
	closeCalls := 0
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		return &nativeStatement{
			planOverride:  func() (string, error) { return "PLAN (GO_COUNTRY NATURAL)", nil },
			closeOverride: func() error { closeCalls++; return nil },
			// The live suite is the real prepare-only proof, but it needs a
			// server. This makes a regression fail fast in `make test`: if a
			// later edit to (*conn).Plan ever reaches an execute path, this
			// fires immediately instead of waiting for the Docker fixture.
			execOverride: func([]argument) (int64, error) {
				t.Fatal("(*conn).Plan executed the statement; it must only prepare")
				return 0, nil
			},
		}, nil
	}
	connection := &conn{native: native}
	plan, err := connection.Plan(context.Background(), "SELECT COUNTRY FROM GO_COUNTRY")
	if err != nil {
		t.Fatalf("(*conn).Plan() error = %v", err)
	}
	if plan != "PLAN (GO_COUNTRY NATURAL)" {
		t.Fatalf("(*conn).Plan() = %q, want the native plan", plan)
	}
	if closeCalls != 1 {
		t.Fatalf("statement close calls = %d, want 1", closeCalls)
	}
}

func TestConnPlanClosesTheStatementAndJoinsBothFailures(t *testing.T) {
	planErr := errors.New("injected plan failure")
	closeErr := errors.New("injected close failure")
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		return &nativeStatement{
			planOverride:  func() (string, error) { return "", planErr },
			closeOverride: func() error { return closeErr },
		}, nil
	}
	connection := &conn{native: native}
	plan, err := connection.Plan(context.Background(), "SELECT COUNTRY FROM GO_COUNTRY")
	if plan != "" {
		t.Fatalf("(*conn).Plan() = %q, want an empty plan on failure", plan)
	}
	if !errors.Is(err, planErr) || !errors.Is(err, closeErr) {
		t.Fatalf("(*conn).Plan() error = %v, want both the plan and close failures", err)
	}
}

func TestConnPlanRejectsInvalidQueriesBeforeNativePrepare(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "NUL byte", query: "SELECT 1 FROM RDB$DATABASE\x00"},
		{name: "oversized", query: strings.Repeat("a", math.MaxUint16+1)},
		{name: "subscription session control", query: "SET SUBSCRIPTION MY_SUB ACTIVE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepareCalls := 0
			native := introspectionTestNative(3)
			native.prepareOverride = func(string) (*nativeStatement, error) {
				prepareCalls++
				return nil, errors.New("native prepare must not be reached")
			}
			connection := &conn{native: native}
			if _, err := connection.Plan(context.Background(), test.query); err == nil {
				t.Fatal("(*conn).Plan() accepted a query the pooled validator rejects")
			}
			if prepareCalls != 0 {
				t.Fatalf("native prepare calls = %d, want 0", prepareCalls)
			}
		})
	}
}

func TestConnPlanInvalidatesTheConnectionWhenNativePrepareBreaksIt(t *testing.T) {
	prepareErr := &NativeError{
		Operation:  "prepare",
		NativeCode: 335544721,
		Message:    "connection lost",
	}
	broken := false
	native := introspectionTestNative(3)
	native.brokenOverride = func() bool { return broken }
	native.prepareOverride = func(string) (*nativeStatement, error) {
		broken = true
		return nil, prepareErr
	}
	connection := &conn{native: native}
	if _, err := connection.Plan(context.Background(), "SELECT COUNTRY FROM GO_COUNTRY"); !errors.Is(err, prepareErr) {
		t.Fatalf("(*conn).Plan() error = %v, want the native prepare failure", err)
	}
	if connection.IsValid() {
		t.Fatal("(*conn).IsValid() is true after a native failure that broke the attachment")
	}
}

func TestConnPlanOnClosedConnectionReturnsErrBadConn(t *testing.T) {
	prepareCalls := 0
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		prepareCalls++
		return nil, errors.New("native prepare must not be reached")
	}
	connection := &conn{native: native}
	if err := connection.Close(); err != nil {
		t.Fatalf("(*conn).Close() error = %v", err)
	}
	if _, err := connection.Plan(context.Background(), "SELECT COUNTRY FROM GO_COUNTRY"); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("(*conn).Plan() error = %v, want driver.ErrBadConn", err)
	}
	if prepareCalls != 0 {
		t.Fatalf("native prepare calls = %d, want 0", prepareCalls)
	}
}
```

Extend the `introspection_test.go` import block to:

```go
import (
	"context"
	"database/sql/driver"
	"errors"
	"math"
	"strings"
	"testing"
)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test . -run 'TestConnPlan' -count=1`

Expected: FAIL — compilation error, `connection.Plan undefined (type *conn has no field or method Plan)`.

- [ ] **Step 3: Implement `(*conn).Plan`**

Append to `introspection.go`, and extend its import block to `"context"`, `"database/sql/driver"`, `"errors"`, `"fmt"`:

```go
// Plan prepares query and returns its server-generated plan without executing
// it, using the attachment behind this connection. The prepared statement is
// never registered with the connection and is always closed before returning.
//
// Native prepare selects the connection's active explicit transaction when
// BeginTx started one and has not completed it, and otherwise starts, uses,
// and completes its own read-only transaction inside the prepare call. Plan
// never begins or completes a caller-owned transaction.
func (c *conn) Plan(ctx context.Context, query string) (string, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if err := validateDatabaseSQLQuery(query); err != nil {
		return "", err
	}
	c.lockDirect()
	defer c.mu.Unlock()
	// Unreachable from the pool by design: c.distributed is only ever assigned
	// through BeginDistributed, which accepts explicit Attachment participants
	// (distributed.go:216). The guard exists because this method lives on the
	// shared *conn. Do not treat it as a live path.
	if c.distributed != nil {
		return "", ErrDistributedParticipantManaged
	}
	if c.closed || c.native == nil || c.native.broken() {
		return "", driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return "", err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return "", err
	}
	defer releaseNative()
	statement, err := c.native.prepare(ctx, query)
	if err != nil {
		releaseNative()
		evidence := nativeCancellationEvidenceOf(err)
		parts := splitNativeExecutionError(err)
		operationErr := c.sanitizeError("prepare plan", parts.primary)
		cleanupErr := c.sanitizeError("", parts.cleanup)
		operationErr = classifyNativeOutcome("prepare plan", false,
			contextCancellation(ctx), operationErr, cleanupErr, evidence)
		operationErr = joinNativeRequestDiagnostic(operationErr,
			c.sanitizeError("", parts.request))
		if c.native.broken() {
			c.invalidateLocked(operationErr)
		}
		return "", operationErr
	}
	if err := contextError(ctx); err != nil {
		return "", errors.Join(err, statement.close())
	}
	plan, planErr := statement.plan()
	closeErr := statement.close()
	releaseNative()
	if planErr != nil || closeErr != nil {
		operationErr := c.sanitizeError("plan", errors.Join(planErr, closeErr))
		if c.native.broken() {
			c.invalidateLocked(operationErr)
		}
		return "", operationErr
	}
	if err := contextError(ctx); err != nil {
		return "", err
	}
	return plan, nil
}
```

Notes for the implementer:

- `releaseNative` is a `sync.OnceFunc` value, so the explicit calls plus the `defer` are safe. `PrepareContext` (`interbase.go:1089-1094`) does the same thing.
- Native prepare already closed its own statement and rolled back its own transaction before returning an error, so there is nothing to clean up on the failure path.
- The prepared statement is never added to `c.statements` and never returned to the caller. It exists only between the `prepare` and `close` calls above.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test . -run 'TestConnPlan' -count=1`

Expected: PASS for all five tests.

- [ ] **Step 5: Commit**

```bash
git add introspection.go introspection_test.go
git commit -m "Add a prepare-only plan method to the driver connection"
```

---

### Task 4: `Introspector`, `ErrNotInterBaseConn`, and the Pooled `Diagnostics` Helper

**Files:**
- Modify: `introspection.go`
- Modify: `introspection_test.go`

**Interfaces:**
- Consumes: `func (c *conn) Diagnostics(ctx context.Context) (DatabaseDiagnostics, error)` (Task 1), `func (c *conn) Plan(ctx context.Context, query string) (string, error)` (Task 3), `openDatabaseSQLTestDB(t *testing.T, connection driver.Conn) *sql.DB` (`connect_timeout_test.go:31`).
- Produces:
  ```go
  type Introspector interface {
      Diagnostics(ctx context.Context) (DatabaseDiagnostics, error)
      Plan(ctx context.Context, query string) (string, error)
  }

  var ErrNotInterBaseConn = errors.New("interbase: connection is not an InterBase connection")

  func Diagnostics(ctx context.Context, conn *sql.Conn) (DatabaseDiagnostics, error)
  ```
- Produces the test type `notInterBaseTestConn` used again in Task 5.

- [ ] **Step 1: Write the failing tests**

Append to `introspection_test.go`:

```go
type notInterBaseTestConn struct{}

func (notInterBaseTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("test connection does not prepare")
}

func (notInterBaseTestConn) Close() error { return nil }

func (notInterBaseTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("test connection does not begin transactions")
}

func TestPooledDiagnosticsDecodesEveryFieldForEachDialect(t *testing.T) {
	for _, dialect := range []byte{1, 3} {
		connection := &conn{native: introspectionTestNative(dialect)}
		db := openDatabaseSQLTestDB(t, connection)
		pooled, err := db.Conn(context.Background())
		if err != nil {
			t.Fatalf("dialect %d: DB.Conn() error = %v", dialect, err)
		}
		got, err := Diagnostics(context.Background(), pooled)
		if err != nil {
			t.Fatalf("dialect %d: Diagnostics() error = %v", dialect, err)
		}
		want := introspectionWantDiagnostics(int64(dialect))
		if got != want {
			t.Fatalf("dialect %d: Diagnostics() = %#v, want %#v", dialect, got, want)
		}
		if err := pooled.Close(); err != nil {
			t.Fatalf("dialect %d: sql.Conn.Close() error = %v", dialect, err)
		}
	}
}

func TestPooledDiagnosticsMatchesAttachmentDiagnostics(t *testing.T) {
	attachment := &Attachment{conn: &conn{native: introspectionTestNative(3)}}
	fromAttachment, err := attachment.Diagnostics(context.Background())
	if err != nil {
		t.Fatalf("Attachment.Diagnostics() error = %v", err)
	}
	db := openDatabaseSQLTestDB(t, &conn{native: introspectionTestNative(3)})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn() error = %v", err)
	}
	defer pooled.Close()
	fromPool, err := Diagnostics(context.Background(), pooled)
	if err != nil {
		t.Fatalf("Diagnostics() error = %v", err)
	}
	if fromAttachment != fromPool {
		t.Fatalf("Attachment.Diagnostics() = %#v, pooled Diagnostics() = %#v; the shared implementation drifted",
			fromAttachment, fromPool)
	}
}

func TestPooledDiagnosticsOnClosedDriverConnectionReturnsErrBadConn(t *testing.T) {
	infoCalls := 0
	native := introspectionTestNative(3)
	responder := introspectionInfoResponder(3)
	native.databaseInfoOverride = func(code byte) ([]byte, error) {
		infoCalls++
		return responder(code)
	}
	connection := &conn{native: native}
	db := openDatabaseSQLTestDB(t, connection)
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn() error = %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("(*conn).Close() error = %v", err)
	}
	if _, err := Diagnostics(context.Background(), pooled); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Diagnostics() error = %v, want driver.ErrBadConn", err)
	}
	if infoCalls != 0 {
		t.Fatalf("database info calls = %d, want 0", infoCalls)
	}
}

func TestPooledDiagnosticsRejectsForeignAndNilConnections(t *testing.T) {
	db := openDatabaseSQLTestDB(t, notInterBaseTestConn{})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn() error = %v", err)
	}
	defer pooled.Close()
	if _, err := Diagnostics(context.Background(), pooled); !errors.Is(err, ErrNotInterBaseConn) {
		t.Fatalf("Diagnostics() on a foreign driver connection error = %v, want ErrNotInterBaseConn", err)
	}
	if _, err := Diagnostics(context.Background(), nil); !errors.Is(err, ErrNotInterBaseConn) {
		t.Fatalf("Diagnostics(nil) error = %v, want ErrNotInterBaseConn", err)
	}
}
```

Extend the `introspection_test.go` import block with `"database/sql"` if the compiler asks for it; `openDatabaseSQLTestDB` returns `*sql.DB` but the tests above never name the type, so it may not be needed.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test . -run 'TestPooledDiagnostics' -count=1`

Expected: FAIL — compilation error, `undefined: Diagnostics` and `undefined: ErrNotInterBaseConn`.

- [ ] **Step 3: Implement the interface, error, and helper**

Prepend to `introspection.go`, after the import block, and add `"database/sql"` to it:

```go
// Introspector is implemented by the driver connection passed to
// (*sql.Conn).Raw. The value must not be retained beyond the callback.
type Introspector interface {
	Diagnostics(ctx context.Context) (DatabaseDiagnostics, error)
	Plan(ctx context.Context, query string) (string, error)
}

var _ Introspector = (*conn)(nil)

// ErrNotInterBaseConn reports a connection that does not belong to this driver.
var ErrNotInterBaseConn = errors.New("interbase: connection is not an InterBase connection")

// Diagnostics returns server, database, and linked client diagnostics for the
// attachment behind a pooled database/sql connection, without opening a second
// native attachment. SQLDialect is the dialect the server reports for this
// attachment, not an echo of Config.Dialect.
//
// The driver error is returned unchanged. A connection that does not belong to
// this driver, including a nil one, returns ErrNotInterBaseConn; a conn that
// has already been closed returns sql.ErrConnDone from Raw.
func Diagnostics(ctx context.Context, conn *sql.Conn) (DatabaseDiagnostics, error) {
	if conn == nil {
		return DatabaseDiagnostics{}, ErrNotInterBaseConn
	}
	var result DatabaseDiagnostics
	if err := conn.Raw(func(driverConn any) error {
		introspector, ok := driverConn.(Introspector)
		if !ok {
			return ErrNotInterBaseConn
		}
		diagnostics, err := introspector.Diagnostics(ctx)
		if err != nil {
			return err
		}
		result = diagnostics
		return nil
	}); err != nil {
		return DatabaseDiagnostics{}, err
	}
	return result, nil
}
```

`var _ Introspector = (*conn)(nil)` compiles only because Task 3 added `(*conn).Plan`; keep the assertion, it is what pins the two methods to the exported contract.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test . -run 'TestPooledDiagnostics' -count=1`

Expected: PASS for all four tests.

- [ ] **Step 5: Commit**

```bash
git add introspection.go introspection_test.go
git commit -m "Expose pooled diagnostics through the Introspector contract"
```

---

### Task 5: Pooled `Plan` Helper

**Files:**
- Modify: `introspection.go`
- Modify: `introspection_test.go`

**Interfaces:**
- Consumes: `Introspector`, `ErrNotInterBaseConn`, `notInterBaseTestConn` (Task 4); `(*conn).Plan` (Task 3); `planOverride` (Task 2).
- Produces: `func Plan(ctx context.Context, conn *sql.Conn, query string) (string, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `introspection_test.go`:

```go
func TestPooledPlanReturnsPlanAndClosesTheStatementExactlyOnce(t *testing.T) {
	closeCalls := 0
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		return &nativeStatement{
			planOverride:  func() (string, error) { return "PLAN (GO_COUNTRY NATURAL)", nil },
			closeOverride: func() error { closeCalls++; return nil },
		}, nil
	}
	db := openDatabaseSQLTestDB(t, &conn{native: native})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn() error = %v", err)
	}
	defer pooled.Close()
	plan, err := Plan(context.Background(), pooled, "SELECT COUNTRY FROM GO_COUNTRY")
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan != "PLAN (GO_COUNTRY NATURAL)" {
		t.Fatalf("Plan() = %q, want the native plan", plan)
	}
	if closeCalls != 1 {
		t.Fatalf("statement close calls = %d, want 1", closeCalls)
	}
}

func TestPooledPlanJoinsPlanAndCloseFailures(t *testing.T) {
	planErr := errors.New("injected plan failure")
	closeErr := errors.New("injected close failure")
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		return &nativeStatement{
			planOverride:  func() (string, error) { return "", planErr },
			closeOverride: func() error { return closeErr },
		}, nil
	}
	db := openDatabaseSQLTestDB(t, &conn{native: native})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn() error = %v", err)
	}
	defer pooled.Close()
	if _, err := Plan(context.Background(), pooled, "SELECT COUNTRY FROM GO_COUNTRY"); err == nil {
		t.Fatal("Plan() succeeded despite a plan and close failure")
	} else if !errors.Is(err, planErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Plan() error = %v, want both the plan and close failures", err)
	}
}

func TestPooledPlanRejectsInvalidQueriesBeforeNativePrepare(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "NUL byte", query: "SELECT 1 FROM RDB$DATABASE\x00"},
		{name: "oversized", query: strings.Repeat("a", math.MaxUint16+1)},
		{name: "subscription session control", query: "SET SUBSCRIPTION MY_SUB ACTIVE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepareCalls := 0
			native := introspectionTestNative(3)
			native.prepareOverride = func(string) (*nativeStatement, error) {
				prepareCalls++
				return nil, errors.New("native prepare must not be reached")
			}
			db := openDatabaseSQLTestDB(t, &conn{native: native})
			pooled, err := db.Conn(context.Background())
			if err != nil {
				t.Fatalf("DB.Conn() error = %v", err)
			}
			defer pooled.Close()
			if _, err := Plan(context.Background(), pooled, test.query); err == nil {
				t.Fatal("Plan() accepted a query the pooled validator rejects")
			}
			if prepareCalls != 0 {
				t.Fatalf("native prepare calls = %d, want 0", prepareCalls)
			}
		})
	}
}

func TestPooledPlanOnClosedDriverConnectionReturnsErrBadConn(t *testing.T) {
	prepareCalls := 0
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		prepareCalls++
		return nil, errors.New("native prepare must not be reached")
	}
	connection := &conn{native: native}
	db := openDatabaseSQLTestDB(t, connection)
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn() error = %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("(*conn).Close() error = %v", err)
	}
	if _, err := Plan(context.Background(), pooled, "SELECT COUNTRY FROM GO_COUNTRY"); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Plan() error = %v, want driver.ErrBadConn", err)
	}
	if prepareCalls != 0 {
		t.Fatalf("native prepare calls = %d, want 0", prepareCalls)
	}
}

func TestPooledPlanRejectsForeignAndNilConnections(t *testing.T) {
	db := openDatabaseSQLTestDB(t, notInterBaseTestConn{})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn() error = %v", err)
	}
	defer pooled.Close()
	if _, err := Plan(context.Background(), pooled, "SELECT 1 FROM RDB$DATABASE"); !errors.Is(err, ErrNotInterBaseConn) {
		t.Fatalf("Plan() on a foreign driver connection error = %v, want ErrNotInterBaseConn", err)
	}
	if _, err := Plan(context.Background(), nil, "SELECT 1 FROM RDB$DATABASE"); !errors.Is(err, ErrNotInterBaseConn) {
		t.Fatalf("Plan(nil) error = %v, want ErrNotInterBaseConn", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test . -run 'TestPooledPlan' -count=1`

Expected: FAIL — compilation error, `undefined: Plan`.

- [ ] **Step 3: Implement the helper**

Append to `introspection.go`, next to `Diagnostics`:

```go
// Plan prepares query on the attachment behind a pooled database/sql
// connection and returns its server-generated plan. It never executes the
// statement, for any statement type, and it opens no second native attachment.
//
// Native prepare uses the connection's active explicit transaction when one
// was started with BeginTx on the same *sql.Conn and has not completed, and
// otherwise uses a read-only transaction that begins and ends inside the
// native prepare call. A valid DML statement may return an empty plan string,
// and an empty plan never implies that the statement ran.
//
// The driver error is returned unchanged. A connection that does not belong to
// this driver, including a nil one, returns ErrNotInterBaseConn; a conn that
// has already been closed returns sql.ErrConnDone from Raw.
func Plan(ctx context.Context, conn *sql.Conn, query string) (string, error) {
	if conn == nil {
		return "", ErrNotInterBaseConn
	}
	var result string
	if err := conn.Raw(func(driverConn any) error {
		introspector, ok := driverConn.(Introspector)
		if !ok {
			return ErrNotInterBaseConn
		}
		plan, err := introspector.Plan(ctx, query)
		if err != nil {
			return err
		}
		result = plan
		return nil
	}); err != nil {
		return "", err
	}
	return result, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test . -run 'TestPooledPlan' -count=1`

Expected: PASS for all five tests.

- [ ] **Step 5: Run the full unit suite**

Run: `go test . -count=1 -timeout=60s`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add introspection.go introspection_test.go
git commit -m "Expose a pooled prepare-only Plan helper"
```

---

### Task 6: Connection-Minting Test Connector and the Pool-Eviction Proof

`databaseSQLTestConnector.Connect` returns one fixed `driver.Conn` on every call (`connect_timeout_test.go:17`), so after the pool drops a dead `*conn` it immediately reconnects to the same dead `*conn`; a pointer-identity assertion against that connector is meaningless. This task adds a sibling connector whose `Connect` calls a `func() driver.Conn` factory and records each minted connection, then uses it to prove the pool really discarded the broken attachment.

**Files:**
- Modify: `connect_timeout_test.go:1-40`
- Modify: `introspection_test.go`

**Interfaces:**
- Consumes: `databaseSQLTestDriver` (`connect_timeout_test.go:25`), `Diagnostics`/`Plan` helpers (Tasks 4–5).
- Produces:
  ```go
  type databaseSQLTestFactoryConnector struct { ... }
  func (c *databaseSQLTestFactoryConnector) Connect(context.Context) (driver.Conn, error)
  func (*databaseSQLTestFactoryConnector) Driver() driver.Driver
  func (c *databaseSQLTestFactoryConnector) mintedConnections() []driver.Conn
  func openDatabaseSQLTestFactoryDB(t *testing.T, factory func() driver.Conn) (*sql.DB, *databaseSQLTestFactoryConnector)
  ```

- [ ] **Step 1: Write the failing tests**

Append to `introspection_test.go`:

```go
// introspectionMintedConn records how often the fake attachment behind one
// minted driver connection was actually asked for information.
type introspectionMintedConn struct {
	connection *conn
	infoCalls  int
	prepares   int
}

func TestPooledDiagnosticsBrokenAttachmentLeavesThePool(t *testing.T) {
	var mu sync.Mutex
	var minted []*introspectionMintedConn
	db, connector := openDatabaseSQLTestFactoryDB(t, func() driver.Conn {
		mu.Lock()
		defer mu.Unlock()
		entry := &introspectionMintedConn{}
		failing := len(minted) == 0
		broken := &atomic.Bool{}
		responder := introspectionInfoResponder(3)
		native := &nativeConnection{
			brokenOverride: broken.Load,
			databaseInfoOverride: func(code byte) ([]byte, error) {
				mu.Lock()
				entry.infoCalls++
				mu.Unlock()
				if failing {
					broken.Store(true)
					return nil, &NativeError{
						Operation:  "database diagnostics",
						NativeCode: 335544721,
						Message:    "connection lost",
					}
				}
				return responder(code)
			},
			clientVersionOverride: func() (string, error) { return "client-v1", nil },
			closeOverride:         func() error { return nil },
		}
		entry.connection = &conn{native: native}
		minted = append(minted, entry)
		return entry.connection
	})

	first, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("first DB.Conn() error = %v", err)
	}
	if _, err := Diagnostics(context.Background(), first); err == nil {
		t.Fatal("Diagnostics() succeeded on a broken attachment")
	}
	mu.Lock()
	brokenConn := minted[0].connection
	mu.Unlock()
	if brokenConn.IsValid() {
		t.Fatal("(*conn).IsValid() is true after a native failure that broke the attachment")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first sql.Conn.Close() error = %v", err)
	}

	second, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("second DB.Conn() error = %v", err)
	}
	defer second.Close()
	got, err := Diagnostics(context.Background(), second)
	if err != nil {
		t.Fatalf("Diagnostics() on the replacement connection error = %v", err)
	}
	if want := introspectionWantDiagnostics(3); got != want {
		t.Fatalf("Diagnostics() = %#v, want %#v", got, want)
	}

	// Asserted as "at least two" rather than exactly two: database/sql mints
	// spare connections only when connRequests is non-empty, which these
	// sequential tests never produce, but a strict equality on pool internals
	// is the kind of assertion that flakes rarely and costs an afternoon. The
	// identity check and the infoCalls count below carry the actual proof.
	if connections := connector.mintedConnections(); len(connections) < 2 {
		t.Fatalf("minted driver connections = %d, want at least 2", len(connections))
	} else if connections[0] == connections[1] {
		t.Fatal("the replacement connection is the discarded one")
	}
	mu.Lock()
	defer mu.Unlock()
	if minted[0].infoCalls != 1 {
		t.Fatalf("broken connection database info calls = %d, want 1; it served a later request",
			minted[0].infoCalls)
	}
	if minted[1].infoCalls == 0 {
		t.Fatal("the replacement connection never served the second Diagnostics call")
	}
}

func TestPooledPlanBrokenAttachmentLeavesThePool(t *testing.T) {
	var mu sync.Mutex
	var minted []*introspectionMintedConn
	db, connector := openDatabaseSQLTestFactoryDB(t, func() driver.Conn {
		mu.Lock()
		defer mu.Unlock()
		entry := &introspectionMintedConn{}
		failing := len(minted) == 0
		broken := &atomic.Bool{}
		native := introspectionTestNative(3)
		native.brokenOverride = broken.Load
		native.prepareOverride = func(string) (*nativeStatement, error) {
			mu.Lock()
			entry.prepares++
			mu.Unlock()
			if failing {
				broken.Store(true)
				return nil, &NativeError{
					Operation:  "prepare",
					NativeCode: 335544721,
					Message:    "connection lost",
				}
			}
			return &nativeStatement{
				planOverride:  func() (string, error) { return "PLAN (GO_COUNTRY NATURAL)", nil },
				closeOverride: func() error { return nil },
			}, nil
		}
		entry.connection = &conn{native: native}
		minted = append(minted, entry)
		return entry.connection
	})

	first, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("first DB.Conn() error = %v", err)
	}
	if _, err := Plan(context.Background(), first, "SELECT COUNTRY FROM GO_COUNTRY"); err == nil {
		t.Fatal("Plan() succeeded on a broken attachment")
	} else if errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Plan() error = %v, want the native failure rather than a manufactured driver.ErrBadConn", err)
	}
	mu.Lock()
	brokenConn := minted[0].connection
	mu.Unlock()
	if brokenConn.IsValid() {
		t.Fatal("(*conn).IsValid() is true after a native failure that broke the attachment")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first sql.Conn.Close() error = %v", err)
	}

	second, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("second DB.Conn() error = %v", err)
	}
	defer second.Close()
	plan, err := Plan(context.Background(), second, "SELECT COUNTRY FROM GO_COUNTRY")
	if err != nil {
		t.Fatalf("Plan() on the replacement connection error = %v", err)
	}
	if plan != "PLAN (GO_COUNTRY NATURAL)" {
		t.Fatalf("Plan() = %q, want the native plan", plan)
	}
	// "At least two" for the same reason as the Diagnostics test above: the
	// identity check and the prepares count are what prove eviction.
	if connections := connector.mintedConnections(); len(connections) < 2 {
		t.Fatalf("minted driver connections = %d, want at least 2", len(connections))
	} else if connections[0] == connections[1] {
		t.Fatal("the replacement connection is the discarded one")
	}
	mu.Lock()
	defer mu.Unlock()
	if minted[0].prepares != 1 {
		t.Fatalf("broken connection prepare calls = %d, want 1; it served a later request", minted[0].prepares)
	}
}
```

Extend the `introspection_test.go` import block with `"sync"` and `"sync/atomic"`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test . -run 'BrokenAttachmentLeavesThePool' -count=1`

Expected: FAIL — compilation error, `undefined: openDatabaseSQLTestFactoryDB`.

- [ ] **Step 3: Add the connection-minting connector**

In `connect_timeout_test.go`, after `openDatabaseSQLTestDB` (`connect_timeout_test.go:31-40`), add:

```go
// databaseSQLTestFactoryConnector mints a fresh driver.Conn per Connect call,
// unlike databaseSQLTestConnector, which returns one fixed connection. Tests
// that must observe the pool discarding a connection need a distinguishable
// replacement.
type databaseSQLTestFactoryConnector struct {
	mu      sync.Mutex
	factory func() driver.Conn
	minted  []driver.Conn
}

func (c *databaseSQLTestFactoryConnector) Connect(context.Context) (driver.Conn, error) {
	connection := c.factory()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minted = append(c.minted, connection)
	return connection, nil
}

func (*databaseSQLTestFactoryConnector) Driver() driver.Driver {
	return databaseSQLTestDriver{}
}

func (c *databaseSQLTestFactoryConnector) mintedConnections() []driver.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]driver.Conn(nil), c.minted...)
}

func openDatabaseSQLTestFactoryDB(t *testing.T, factory func() driver.Conn) (*sql.DB,
	*databaseSQLTestFactoryConnector) {
	t.Helper()
	connector := &databaseSQLTestFactoryConnector{factory: factory}
	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("closing database/sql test DB: %v", err)
		}
	})
	return db, connector
}
```

Add `"sync"` to the `connect_timeout_test.go` import block (`connect_timeout_test.go:3-11`), which currently has `context`, `database/sql`, `database/sql/driver`, `errors`, `net`, `testing`, `time`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test . -run 'BrokenAttachmentLeavesThePool' -count=1`

Expected: PASS for both tests.

- [ ] **Step 5: Run the full unit suite under the race detector**

Run: `go test -race . -count=1 -timeout=120s`

Expected: PASS with no race reports. The factory closures are shared between the test goroutine and any `database/sql` connection-opener goroutine, which is why the minted state is mutex- and atomic-guarded.

- [ ] **Step 6: Commit**

```bash
git add connect_timeout_test.go introspection_test.go
git commit -m "Prove a broken introspection connection leaves the pool"
```

---

### Task 7: Cancellation During a Pooled `Plan`

`Plan` adds no cancellation guarantee; this task proves it inherits the existing one and that the connection mutex is released afterwards. It follows the `t.Run("prepare", ...)` pattern in `cancellation_test.go:888-921`, which already drives `prepareContextOverride` with a `fakeCancelSlot`.

**Files:**
- Modify: `introspection_test.go`

**Interfaces:**
- Consumes: `fakeCancelSlot` (`cancellation_test.go:16`), `waitForTestSignal(t *testing.T, signal <-chan struct{}, message string)` (`cancellation_test.go:93`), `nativeCancelledCode` (`native.go:27`), `NativeError` (`cancellation.go:16`, an alias for `Error`), `prepareContextOverride func(string, *nativeCancelOperation) (*nativeStatement, error)` and `cancelSlotOverride nativeCancelSlotBackend` (`native.go:193-194`), `openDatabaseSQLTestDB` (`connect_timeout_test.go:31`).
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the failing test**

Append to `introspection_test.go`:

```go
func TestPooledPlanCancellationReleasesTheConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	primary := &NativeError{
		Operation:  "prepare",
		NativeCode: nativeCancelledCode,
		Message:    "isc_cancelled",
	}
	slot := &fakeCancelSlot{
		cancelStarted:    make(chan struct{}),
		cancelAttempted:  true,
		cancelOverlapped: true,
	}
	native := introspectionTestNative(3)
	native.cancelSlotOverride = slot
	native.prepareContextOverride = func(_ string, _ *nativeCancelOperation) (*nativeStatement, error) {
		close(entered)
		<-ctx.Done()
		waitForTestSignal(t, slot.cancelStarted, "pooled Plan cancellation did not overlap the native call")
		return nil, primary
	}
	connection := &conn{native: native}
	db := openDatabaseSQLTestDB(t, connection)
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn() error = %v", err)
	}
	defer pooled.Close()

	result := make(chan error, 1)
	go func() {
		_, planErr := Plan(ctx, pooled, "SELECT COUNTRY FROM GO_COUNTRY")
		result <- planErr
	}()
	waitForTestSignal(t, entered, "pooled Plan did not enter the native prepare override")
	cancel()
	waitForTestSignal(t, slot.cancelStarted, "pooled Plan cancellation did not run")

	planErr := <-result
	if !errors.Is(planErr, context.Canceled) {
		t.Fatalf("Plan() error = %v, want the originating context", planErr)
	}
	var gotNative *NativeError
	if !errors.As(planErr, &gotNative) || gotNative.NativeCode != nativeCancelledCode {
		t.Fatalf("Plan() error = %v, want native cancellation diagnostics", planErr)
	}
	if errors.Is(planErr, driver.ErrBadConn) {
		t.Fatal("pooled Plan cancellation was classified as driver.ErrBadConn")
	}

	// A completing call on the same *conn proves c.mu was released. The fixed
	// test connector hands back the same driver connection, so this really is
	// the connection the canceled Plan used.
	got, err := Diagnostics(context.Background(), pooled)
	if err != nil {
		t.Fatalf("Diagnostics() after a canceled Plan error = %v", err)
	}
	if want := introspectionWantDiagnostics(3); got != want {
		t.Fatalf("Diagnostics() = %#v, want %#v", got, want)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test . -run 'TestPooledPlanCancellationReleasesTheConnection' -count=1`

Expected: FAIL.

If it fails because the cancellation error is reported without the native diagnostics, or because `Diagnostics` afterwards returns `driver.ErrBadConn`, the `(*conn).Plan` error path from Task 3 is wrong — re-read `interbase.go:1094-1118` and fix `introspection.go`, not the test. If it fails only on timing (`waitForTestSignal` fires), the override ordering in the test is wrong; compare against `cancellation_test.go:888-921`.

- [ ] **Step 3: Make it pass**

No production change is expected: Task 3 already routes prepare failures through `classifyNativeOutcome("prepare plan", false, contextCancellation(ctx), ...)` and never calls `invalidateLocked` unless `c.native.broken()` reports true, which `introspectionTestNative` does not. If the test passes on first run after Step 2's RED was a genuine compile/behavior failure, record that and move on; if Step 2 revealed a real defect, fix `(*conn).Plan` in `introspection.go` now.

- [ ] **Step 4: Run the test and the race detector**

Run:

```sh
go test . -run 'TestPooledPlanCancellationReleasesTheConnection' -count=1
go test -race . -run 'TestPooledPlan|TestPooledDiagnostics' -count=20 -timeout=180s
```

Expected: PASS, no race reports, no flakes across 20 iterations.

- [ ] **Step 5: Commit**

```bash
git add introspection_test.go
git commit -m "Cover cancellation during a pooled plan"
```

---

### Task 8: Live Introspection Tests

Every test here needs a disposable server and is therefore behind `//go:build integration`. They run under `make test-integration-docker`.

**Files:**
- Create: `integration/introspection_test.go`

**Interfaces:**
- Consumes: `newDatabaseWithDialect(t *testing.T, dialect int) *sql.DB` (`integration/helpers_test.go:148`), `createFixture(t *testing.T, dialect int, schemaOverride ...string) (*testfixture.Database, testfixture.Config, *fixtureCleanup)` (`integration/helpers_test.go:173`), `openDatabase(t *testing.T, cleanup *fixtureCleanup, database, user, password, charset string, dialect int, transactionOptions interbase.TransactionOptions) *sql.DB` (`integration/helpers_test.go:205`), `readContext(t *testing.T) context.Context` (`integration/read_test.go:26`), the fixture tables `GO_COUNTRY` and `GO_WRITE` (`integration/testdata/schema.sql:1-5`), and the public `interbase.Diagnostics`/`interbase.Plan` helpers.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the failing live tests**

Create `integration/introspection_test.go`:

```go
//go:build integration

package integration_test

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	interbase "interbase-go"
)

func TestIntrospectionDiagnosticsReportsDialect(t *testing.T) {
	for _, dialect := range []int{1, 3} {
		t.Run(fmt.Sprintf("dialect%d", dialect), func(t *testing.T) {
			db := newDatabaseWithDialect(t, dialect)
			ctx := readContext(t)
			pooled, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("DB.Conn(): %v", err)
			}
			defer pooled.Close()

			diagnostics, err := interbase.Diagnostics(ctx, pooled)
			if err != nil {
				t.Fatalf("pooled Diagnostics(): %v", err)
			}
			if diagnostics.SQLDialect != int64(dialect) {
				t.Fatalf("SQLDialect = %d, want %d", diagnostics.SQLDialect, dialect)
			}
			if diagnostics.ClientVersion == "" || diagnostics.ServerVersion == "" ||
				diagnostics.PageSize <= 0 {
				t.Fatalf("Diagnostics = %#v, missing expected native values", diagnostics)
			}
		})
	}
}

func TestIntrospectionPlanReturnsSelectPlan(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	pooled, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	defer pooled.Close()

	plan, err := interbase.Plan(ctx, pooled, "SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?")
	if err != nil {
		t.Fatalf("pooled Plan(): %v", err)
	}
	if plan == "" {
		t.Fatal("pooled Plan() returned an empty plan for a SELECT")
	}
}

// TestIntrospectionPlanForDMLDoesNotExecuteImplicitly establishes only that the
// read-only default path is safe. With no explicit transaction the native
// prepare transaction is read-only, so a hypothetical execute would be refused
// by the engine and these assertions would pass for the wrong reason.
// TestIntrospectionPlanInsideWritableTransaction removes that escape.
func TestIntrospectionPlanForDMLDoesNotExecuteImplicitly(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	first := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	second := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	ctx := readContext(t)

	pooled, err := first.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	defer pooled.Close()

	var sentinel string
	if err := pooled.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&sentinel); err != nil {
		t.Fatalf("read sentinel row: %v", err)
	}
	if sentinel != "USA" {
		t.Fatalf("sentinel COUNTRY = %q, want USA", sentinel)
	}

	for _, statement := range []string{
		"UPDATE GO_COUNTRY SET COUNTRY = 'changed' WHERE ID = 1",
		"DELETE FROM GO_COUNTRY WHERE ID = 1",
	} {
		plan, err := interbase.Plan(ctx, pooled, statement)
		if err != nil {
			t.Fatalf("pooled Plan(%q): %v", statement, err)
		}
		// An empty DML plan is permitted, so the plan string is evidence of
		// nothing and is only logged.
		t.Logf("plan for %q: %q", statement, plan)
	}

	var country string
	if err := pooled.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("re-read sentinel row: %v", err)
	}
	if country != "USA" {
		t.Fatalf("COUNTRY after planning DML = %q, want USA", country)
	}
	var rowCount int64
	if err := pooled.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&rowCount); err != nil {
		t.Fatalf("count sentinel row: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("row count for ID = 1 after planning DML = %d, want 1", rowCount)
	}

	var separateCountry string
	if err := second.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&separateCountry); err != nil {
		t.Fatalf("read sentinel row from the second attachment: %v", err)
	}
	if separateCountry != "USA" {
		t.Fatalf("COUNTRY from the second attachment = %q, want USA", separateCountry)
	}
}

func TestIntrospectionPlanLeavesNoOpenTransaction(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	first := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	second := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	ctx := readContext(t)

	pooled, err := first.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	defer pooled.Close()

	if _, err := interbase.Plan(ctx, pooled, "SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?"); err != nil {
		t.Fatalf("pooled Plan(): %v", err)
	}
	if _, err := pooled.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(9101), int64(91), "after plan"); err != nil {
		t.Fatalf("insert after pooled Plan(): %v", err)
	}

	var label string
	if err := second.QueryRowContext(ctx,
		"SELECT LABEL FROM GO_WRITE WHERE ID = ?", int64(9101)).Scan(&label); err != nil {
		t.Fatalf("read the committed row from the second attachment: %v", err)
	}
	if label != "after plan" {
		t.Fatalf("LABEL = %q, want \"after plan\"", label)
	}
}

func TestIntrospectionOnClosedConn(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	pooled, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	if err := pooled.Close(); err != nil {
		t.Fatalf("sql.Conn.Close(): %v", err)
	}
	if _, err := interbase.Diagnostics(ctx, pooled); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("Diagnostics() on a closed conn = %v, want sql.ErrConnDone", err)
	}
	if _, err := interbase.Plan(ctx, pooled,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?"); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("Plan() on a closed conn = %v, want sql.ErrConnDone", err)
	}
}
```

- [ ] **Step 2: Write the load-bearing safety test**

This is the proof that `Plan` never executes, and it must not be collapsed into a smaller test: the writable explicit transaction is what makes an execute both permitted and visible, and every statement form runs inside that one transaction. Append to `integration/introspection_test.go`:

```go
// TestIntrospectionPlanInsideWritableTransaction carries both the
// transaction-selection check and the load-bearing safety check, because both
// need the same writable explicit transaction.
func TestIntrospectionPlanInsideWritableTransaction(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	first := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	second := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	ctx := readContext(t)

	pooled, err := first.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	defer pooled.Close()

	tx, err := pooled.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("sql.Conn.BeginTx(): %v", err)
	}
	rolledBack := false
	defer func() {
		if !rolledBack {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx,
		"CREATE TABLE GO_INTROSPECTION_TXPROBE (ID INTEGER NOT NULL PRIMARY KEY, TEXT_VALUE VARCHAR(32))"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO GO_INTROSPECTION_TXPROBE (ID, TEXT_VALUE) VALUES (1, 'sentinel')"); err != nil {
		t.Fatalf("insert probe sentinel: %v", err)
	}

	// Transaction-selection proof: the table and row are still uncommitted, so
	// a Plan that started its own implicit transaction could not see the
	// metadata and prepare would fail with an unknown-table error.
	plan, err := interbase.Plan(ctx, pooled,
		"SELECT TEXT_VALUE FROM GO_INTROSPECTION_TXPROBE WHERE ID = 1")
	if err != nil {
		t.Fatalf("pooled Plan() inside the writable transaction: %v", err)
	}
	if plan == "" {
		t.Fatal("pooled Plan() returned an empty plan for a SELECT on the uncommitted probe table")
	}

	// Safety proof: the transaction the DML would have run in is writable and
	// reads its own uncommitted work, so an execute would be both permitted
	// and visible. The reads go through tx because the probe table is not
	// visible outside this transaction.
	mutations := []string{
		"UPDATE GO_INTROSPECTION_TXPROBE SET TEXT_VALUE = 'changed' WHERE ID = 1",
		"DELETE FROM GO_INTROSPECTION_TXPROBE WHERE ID = 1",
		"INSERT INTO GO_INTROSPECTION_TXPROBE VALUES (2, 'inserted')",
	}
	for _, statement := range mutations {
		mutationPlan, planErr := interbase.Plan(ctx, pooled, statement)
		if planErr != nil {
			t.Fatalf("pooled Plan(%q): %v", statement, planErr)
		}
		t.Logf("plan for %q: %q", statement, mutationPlan)
		assertTxProbeUnchanged(t, ctx, tx, fmt.Sprintf("after planning %q", statement))
	}

	// The procedure form is called out separately because the execution path
	// does treat procedures specially: ib_connection_query re-prepares an
	// implicit procedure under a write transaction (native.c:7486), while
	// ib_statement_prepare_mode has no such branch.
	if _, err := tx.ExecContext(ctx, `CREATE PROCEDURE GO_INTROSPECTION_TXPROC AS
BEGIN
  INSERT INTO GO_INTROSPECTION_TXPROBE (ID, TEXT_VALUE) VALUES (3, 'procedure');
END`); err != nil {
		t.Fatalf("create probe procedure: %v", err)
	}
	procedurePlan, err := interbase.Plan(ctx, pooled, "EXECUTE PROCEDURE GO_INTROSPECTION_TXPROC")
	if err != nil {
		t.Fatalf("pooled Plan(EXECUTE PROCEDURE): %v", err)
	}
	t.Logf("plan for EXECUTE PROCEDURE GO_INTROSPECTION_TXPROC: %q", procedurePlan)
	assertTxProbeUnchanged(t, ctx, tx, "after planning EXECUTE PROCEDURE")

	// A sql.ErrTxDone here would mean Plan had already completed the caller's
	// transaction, which would make every assertion above vacuous.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("sql.Tx.Rollback() = %v, want nil; pooled Plan completed the caller's transaction", err)
	}
	rolledBack = true

	var relationCount int64
	if err := second.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = ?",
		"GO_INTROSPECTION_TXPROBE").Scan(&relationCount); err != nil {
		t.Fatalf("look up the probe table from the second attachment: %v", err)
	}
	if relationCount != 0 {
		t.Fatalf("GO_INTROSPECTION_TXPROBE relation count after rollback = %d, want 0", relationCount)
	}
}

func assertTxProbeUnchanged(t *testing.T, ctx context.Context, tx *sql.Tx, stage string) {
	t.Helper()
	var sentinel string
	if err := tx.QueryRowContext(ctx,
		"SELECT TEXT_VALUE FROM GO_INTROSPECTION_TXPROBE WHERE ID = 1").Scan(&sentinel); err != nil {
		t.Fatalf("%s: read probe sentinel: %v", stage, err)
	}
	if sentinel != "sentinel" {
		t.Fatalf("%s: TEXT_VALUE for ID = 1 = %q, want \"sentinel\"", stage, sentinel)
	}
	var rowCount int64
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM GO_INTROSPECTION_TXPROBE").Scan(&rowCount); err != nil {
		t.Fatalf("%s: count probe rows: %v", stage, err)
	}
	if rowCount != 1 {
		t.Fatalf("%s: probe row count = %d, want 1", stage, rowCount)
	}
}
```

Add `"context"` to the `integration/introspection_test.go` import block for the `assertTxProbeUnchanged` signature.

- [ ] **Step 3: Verify the file compiles and `make test` stays server-free**

Run:

```sh
go vet -tags=integration ./...
go test . -count=1 -timeout=60s
```

Expected: `go vet` clean; the untagged unit run passes without a server and does not build the new live file.

- [ ] **Step 4: Run the live tests**

Run:

```sh
make test-integration-docker GO_TEST_ARGS="-run '^TestIntrospection' -v"
```

Expected: PASS for all six live tests.

Triage guidance if one fails:
- `TestIntrospectionPlanInsideWritableTransaction` failing at step "pooled Plan() inside the writable transaction" with an unknown-table error means native prepare did **not** select the connection's active explicit transaction. That contradicts the spec's reading of `ib_statement_prepare_mode` (`native.c:7801`); stop and report it rather than weakening the test.
- Any `assertTxProbeUnchanged` failure means `Plan` executed something. Stop and report it; that is the failure this whole sub-project exists to exclude.
- A failure only on the `RDB$RELATIONS` lookup is a query-shape problem, not a safety problem: confirm the relation name comparison against the padded `CHAR` column on this server before changing anything else.
- **Fixture setup failure — pre-authorized fallback.** This test seeds and then mutates `GO_INTROSPECTION_TXPROBE` inside the *same uncommitted* transaction as its `CREATE TABLE`, and creates a procedure referencing that table after DML has touched it. Nothing in this repo proves InterBase permits that, and the existing precedent hedges the other way: `createLifecycleTable` commits the `CREATE TABLE` before a fresh transaction runs the `INSERT` (`integration/lifecycle_test.go:106-124`). If `t.Fatalf("insert probe sentinel: ...")` or the `CREATE PROCEDURE` fails with a metadata or object-in-use error, that is a *fixture* limitation, not a finding about `Plan`.

  Restructure as follows rather than weakening any assertion. Both properties survive intact, and neither the transaction-selection proof nor the safety proof is reduced:

  1. Before `BeginTx`, create `GO_INTROSPECTION_PROBE` with the sentinel row and **commit** it, following the `createLifecycleTable` pattern. Run the UPDATE, DELETE, INSERT and `EXECUTE PROCEDURE` legs against this committed table from inside the writable transaction. The safety proof is unchanged: the transaction is writable and its read-back sees its own uncommitted work, so an execute would be both permitted and visible.
  2. Inside the writable transaction, `CREATE TABLE GO_INTROSPECTION_TXMETA (ID INTEGER)` and leave it uncommitted, used *only* as the transaction-selection signal — `Plan` a `SELECT` against it and require a non-empty plan. Uncommitted metadata is invisible to any other transaction, so this still fails with unknown-table if `Plan` started its own.
  3. Keep step 5 unchanged: `tx.Rollback()` must return nil, and both `GO_INTROSPECTION_TXMETA` and the mutations must be absent from the second pool.

  Do **not** respond to a fixture failure by dropping the procedure leg, by moving the DML legs outside the writable transaction, or by relaxing the non-empty-plan requirement. Those are the three changes that would silently hollow out the proof.

- [ ] **Step 5: Commit**

```bash
git add integration/introspection_test.go
git commit -m "Add live pooled introspection tests"
```

---

### Task 9: README Documentation and Final Verification

**Files:**
- Modify: `README.md:80-130` ("Go Usage"), `README.md:320-421` ("Supported Boundary"), `README.md:422-516` ("Known Limits")

**Interfaces:**
- Consumes: the public `Introspector`, `ErrNotInterBaseConn`, `Diagnostics`, and `Plan` surface from Tasks 4–5.
- Produces: no code.

- [ ] **Step 1: Add the pooled introspection example to "Go Usage"**

Insert after the `Config.TransactionOptions` paragraph that ends the "Go Usage" prose (immediately before the `### Explicit direct API` heading at `README.md:132`):

````markdown
Pooled connections can reach two introspection capabilities without opening a
second native attachment. Borrow a connection with `db.Conn`, then call the
package helpers:

```go
pooled, err := db.Conn(ctx)
if err != nil {
    return err
}
defer pooled.Close()

diagnostics, err := interbase.Diagnostics(ctx, pooled)
if err != nil {
    return err
}
// diagnostics.SQLDialect is the dialect the server reports for this
// attachment, not an echo of Config.Dialect.

plan, err := interbase.Plan(ctx, pooled, "SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?")
if err != nil {
    return err
}
```

Both helpers run on the attachment the pool already owns. They wrap
`(*sql.Conn).Raw` and assert the exported `Introspector` interface; a
connection that does not belong to this driver returns `ErrNotInterBaseConn`.
The `Introspector` value handed to a `Raw` callback must not be retained beyond
that callback.
````

- [ ] **Step 2: Add the "Supported Boundary" entry**

Insert after the "Reusable server-side prepared statements via `PrepareContext`." bullet (`README.md:334`):

```markdown
- Pooled introspection on a connection borrowed with `db.Conn`:
  `Diagnostics` reports the server, database, and linked client facts for the
  attachment the pool already owns, and `Plan` returns a server-generated plan.
  Pooled `Plan` prepares only: it uses the connection's explicit transaction
  when `BeginTx` started one on the same `*sql.Conn` and has not completed it,
  and otherwise uses a read-only transaction that begins and ends inside the
  native prepare call. It never executes the statement, for any statement type,
  and it neither begins nor completes a caller-owned transaction.
```

- [ ] **Step 3: Add the "Known Limits" entry**

Insert after the "No named parameters..." bullet (`README.md:430-432`):

```markdown
- Pooled `Plan` uses the connection-level prepare, which does not allow array
  output columns. A `SELECT` whose output includes an array column therefore
  fails output-type validation instead of returning a plan, exactly as
  `(*sql.DB).PrepareContext` already does with the same query. The direct
  `Transaction.Plan` is array-capable. Separately, a valid DML statement may
  return an empty plan string, and an empty plan never means the statement ran.
```

- [ ] **Step 4: Run the complete verification**

Run serially, with the official SDK/client environment:

```sh
make test INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include INTERBASE_LIB=/tmp/opencode
go test -race . -count=1 -timeout=180s
go vet -tags=integration ./...
make build INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include INTERBASE_LIB=/tmp/opencode
make test-integration-docker GO_TEST_ARGS="-run '^TestIntrospection' -v"
git diff --check
```

Use `LD_LIBRARY_PATH=/tmp/opencode` for the `go test -race` command when linked against a temporary client. Record the actual output; do not claim a result that was not observed.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "Document pooled introspection"
```

- [ ] **Step 6: Inspect the final worktree**

Run `git status --short` and confirm nothing unintended is staged or left untracked, and that the only modified files across the nine commits are `introspection.go`, `introspection_test.go`, `direct.go`, `native.go`, `connect_timeout_test.go`, `integration/introspection_test.go`, and `README.md`.
