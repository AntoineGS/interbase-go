package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewConnectorRejectsIncompleteConfigWithoutLeakingPassword(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "missing database",
			cfg:  Config{User: "alice", Password: "super-secret"},
		},
		{
			name: "missing user",
			cfg:  Config{Database: "/tmp/example.ib", Password: "super-secret"},
		},
		{
			name: "database exceeds attach length",
			cfg: Config{
				Database: strings.Repeat("d", math.MaxInt16+1),
				User:     "alice",
				Password: "super-secret",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewConnector(tc.cfg)
			if err == nil {
				t.Fatal("NewConnector returned nil error for invalid config")
			}
			if strings.Contains(err.Error(), tc.cfg.Password) {
				t.Fatalf("error leaked password: %q", err)
			}
		})
	}
}

func TestConfigCharsetNormalizesSupportedValues(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty defaults to UTF8", input: "", want: "UTF8"},
		{name: "UTF8", input: "utf8", want: "UTF8"},
		{name: "NONE", input: "none", want: "NONE"},
		{name: "WIN1250", input: "win1250", want: "WIN1250"},
		{name: "WIN1252", input: "win1252", want: "WIN1252"},
		{name: "ISO8859_1", input: "iso8859_1", want: "ISO8859_1"},
		{name: "ASCII", input: "ascii", want: "ASCII"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeCharset(tc.input)
			if err != nil {
				t.Fatalf("normalizeCharset(%q) returned error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeCharset(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestConfigDialectDefaultsAndValidation(t *testing.T) {
	field, ok := reflect.TypeOf(Config{}).FieldByName("Dialect")
	if !ok {
		t.Fatal("Config.Dialect is missing")
	}
	if field.Type != reflect.TypeOf(int(0)) {
		t.Fatalf("Config.Dialect type = %v, want int", field.Type)
	}

	base := Config{
		Database: "/tmp/example.ib",
		User:     "alice",
		Password: "super-secret",
	}
	for _, test := range []struct {
		name  string
		value int
		want  int
	}{
		{name: "zero selects dialect three", value: 0, want: 3},
		{name: "explicit dialect one", value: 1, want: 1},
		{name: "explicit dialect three", value: 3, want: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			reflect.ValueOf(&cfg).Elem().FieldByName("Dialect").SetInt(int64(test.value))
			opened, err := NewConnector(cfg)
			if err != nil {
				t.Fatalf("NewConnector returned error: %v", err)
			}
			got := int(reflect.ValueOf(opened.(*connector).cfg).FieldByName("Dialect").Int())
			if got != test.want {
				t.Fatalf("resolved dialect = %d, want %d", got, test.want)
			}
		})
	}

	for _, value := range []int{-1, 2, 4, 99} {
		t.Run("reject-"+strconv.Itoa(value), func(t *testing.T) {
			cfg := base
			reflect.ValueOf(&cfg).Elem().FieldByName("Dialect").SetInt(int64(value))
			if connector, err := NewConnector(cfg); connector != nil || err == nil {
				t.Fatalf("NewConnector(%d) = (%v, %v), want upfront validation error", value, connector, err)
			}
		})
	}
}

func TestNewConnectorRejectsUnsupportedCharset(t *testing.T) {
	_, err := NewConnector(Config{
		Database: "/tmp/example.ib",
		User:     "alice",
		Password: "super-secret",
		Charset:  "NOPE",
	})
	if err == nil {
		t.Fatal("NewConnector accepted unsupported charset")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked password: %q", err)
	}
}

func TestDatabaseSQLRejectsSubscriptionSessionControl(t *testing.T) {
	queries := []string{
		"SET SUBSCRIPTION SUB_CUSTOMER_CHANGE ACTIVE",
		"set   subscription SUB_CUSTOMER_CHANGE inactive",
		"-- configure the change view\n SET /* keep the session isolated */ SUBSCRIPTION SUB_CUSTOMER_CHANGE ACTIVE",
	}
	for _, query := range queries {
		_, err := convertQuery(query, nil)
		if err == nil {
			t.Fatalf("convertQuery(%q) accepted subscription session control", query)
		}
		if !strings.Contains(err.Error(), "explicit direct API") {
			t.Fatalf("convertQuery(%q) error = %v, want explicit direct API guidance", query, err)
		}
	}
}

func TestDirectQueryValidationAllowsSubscriptionSessionControl(t *testing.T) {
	if err := validateQueryText("SET SUBSCRIPTION SUB_CUSTOMER_CHANGE ACTIVE"); err != nil {
		t.Fatalf("validateQueryText rejected direct subscription control: %v", err)
	}
}

type countingValuer struct {
	calls *int
	value driver.Value
}

func (v countingValuer) Value() (driver.Value, error) {
	(*v.calls)++
	return v.value, nil
}

func TestCheckNamedValueNormalizesValuerOnce(t *testing.T) {
	calls := 0
	named := driver.NamedValue{
		Ordinal: 1,
		Value: countingValuer{
			calls: &calls,
			value: int64(42),
		},
	}

	if err := (&conn{}).CheckNamedValue(&named); err != nil {
		t.Fatalf("CheckNamedValue returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Valuer calls after CheckNamedValue = %d, want 1", calls)
	}
	if got, ok := named.Value.(int64); !ok || got != 42 {
		t.Fatalf("normalized NamedValue.Value = %#v, want int64(42)", named.Value)
	}

	if _, err := convertNamedValues([]driver.NamedValue{named}); err != nil {
		t.Fatalf("convertNamedValues returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Valuer calls after conversion = %d, want 1", calls)
	}
}

func TestConnectorConnectHonorsPreCancelledContext(t *testing.T) {
	connector, err := NewConnector(Config{
		Database: "/tmp/example.ib",
		User:     "alice",
		Password: "super-secret",
	})
	if err != nil {
		t.Fatalf("NewConnector returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	conn, err := connector.Connect(ctx)
	if conn != nil {
		t.Fatal("Connect returned a connection after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect error = %v, want context.Canceled", err)
	}
}

func TestConvertArgumentSupportsPoCBoundaryTypes(t *testing.T) {
	tests := []struct {
		name       string
		input      any
		wantKind   argumentKind
		wantString string
		wantInt64  int64
		wantFloat  float64
		wantBool   bool
	}{
		{
			name:       "nil",
			input:      nil,
			wantKind:   argumentNull,
			wantString: "",
		},
		{
			name:       "string preserves bytes",
			input:      "  hello  ",
			wantKind:   argumentString,
			wantString: "  hello  ",
		},
		{
			name:      "int64",
			input:     int64(-42),
			wantKind:  argumentInt64,
			wantInt64: -42,
		},
		{
			name:      "float64",
			input:     math.Pi,
			wantKind:  argumentFloat64,
			wantFloat: math.Pi,
		},
		{
			name:     "bool",
			input:    true,
			wantKind: argumentBool,
			wantBool: true,
		},
		{
			name:      "int converts to int64",
			input:     int(7),
			wantKind:  argumentInt64,
			wantInt64: 7,
		},
		{
			name:      "int32 converts to int64",
			input:     int32(-8),
			wantKind:  argumentInt64,
			wantInt64: -8,
		},
		{
			name:      "float32 converts to float64",
			input:     float32(1.25),
			wantKind:  argumentFloat64,
			wantFloat: float64(float32(1.25)),
		},
		{
			name:     "timestamp",
			input:    time.Date(2024, time.February, 29, 12, 34, 56, 700000, time.UTC),
			wantKind: argumentTimestamp,
		},
		{
			name:     "bytes",
			input:    []byte{0, 1, 2},
			wantKind: argumentBytes,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := convertArgument(tc.input)
			if err != nil {
				t.Fatalf("convertArgument returned error: %v", err)
			}
			if got.kind != tc.wantKind {
				t.Fatalf("kind = %v, want %v", got.kind, tc.wantKind)
			}
			if got.stringValue != tc.wantString {
				t.Errorf("stringValue = %q, want %q", got.stringValue, tc.wantString)
			}
			if got.int64Value != tc.wantInt64 {
				t.Errorf("int64Value = %d, want %d", got.int64Value, tc.wantInt64)
			}
			if got.float64Value != tc.wantFloat {
				t.Errorf("float64Value = %v, want %v", got.float64Value, tc.wantFloat)
			}
			if got.boolValue != tc.wantBool {
				t.Errorf("boolValue = %t, want %t", got.boolValue, tc.wantBool)
			}
			if tc.wantKind == argumentTimestamp && !got.timeValue.Equal(tc.input.(time.Time)) {
				t.Errorf("timeValue = %v, want %v", got.timeValue, tc.input)
			}
			if tc.wantKind == argumentBytes && !strings.EqualFold(string(got.bytesValue), string(tc.input.([]byte))) {
				t.Errorf("bytesValue = %v, want %v", got.bytesValue, tc.input)
			}
		})
	}
}

func TestConvertArgumentRejectsTypesOutsidePoCBoundary(t *testing.T) {
	for _, input := range []any{
		struct{}{},
	} {
		if _, err := convertArgument(input); err == nil {
			t.Fatalf("convertArgument(%T) returned nil error", input)
		}
	}
}

func TestConvertArgumentRejectsOversizedString(t *testing.T) {
	arg, err := convertArgument(strings.Repeat("x", math.MaxInt16+1))
	if err != nil {
		t.Fatalf("convertArgument rejected a large BLOB-capable string: %v", err)
	}
	if arg.kind != argumentString || len(arg.stringValue) != math.MaxInt16+1 {
		t.Fatalf("large string argument = %#v, want string length %d", arg, math.MaxInt16+1)
	}
}

func TestConvertNamedValuesRejectsTooManyParameters(t *testing.T) {
	values := make([]driver.NamedValue, math.MaxInt16+1)
	if _, err := convertNamedValues(values); err == nil {
		t.Fatal("convertNamedValues accepted more parameters than SQLDA supports")
	}
}

func TestFormatScaledIntegerPreservesDecimalPrecision(t *testing.T) {
	tests := []struct {
		name  string
		value int64
		scale int16
		want  string
	}{
		{name: "negative hundredths", value: -123, scale: -2, want: "-1.23"},
		{name: "leading fractional zero", value: 5, scale: -2, want: "0.05"},
		{name: "trailing fractional zero", value: 100, scale: -2, want: "1.00"},
		{name: "positive scale", value: 12, scale: 2, want: "1200"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := formatScaledInteger(tc.value, tc.scale)
			if err != nil {
				t.Fatalf("formatScaledInteger returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("formatScaledInteger = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatScaledIntegerRejectsUnreasonableScale(t *testing.T) {
	if _, err := formatScaledInteger(1, -10001); err == nil {
		t.Fatal("formatScaledInteger accepted an unreasonable scale")
	}
}

func TestSanitizeErrorRedactsConnectionDetails(t *testing.T) {
	err := sanitizeError(
		"connect",
		errors.New("failed interbase://alice:super-secret@/tmp/private.ib"),
		"/tmp/private.ib",
		"alice",
		"super-secret",
	)
	message := err.Error()
	for _, secret := range []string{"/tmp/private.ib", "alice", "super-secret"} {
		if strings.Contains(message, secret) {
			t.Fatalf("error contains connection detail %q: %q", secret, message)
		}
	}
}

func TestConnRejectsUnavailableStateChangingOperations(t *testing.T) {
	conn := new(conn)
	ctx := context.Background()

	if _, err := conn.Prepare("SELECT 1"); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Prepare error = %v, want driver.ErrBadConn", err)
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM secret", nil); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("ExecContext error = %v, want driver.ErrBadConn", err)
	}
	if _, err := conn.Begin(); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Begin error = %v, want driver.ErrBadConn", err)
	}
}

func TestConnRejectsUnsupportedIsolation(t *testing.T) {
	conn := new(conn)
	_, err := conn.BeginTx(context.Background(), driver.TxOptions{
		Isolation: driver.IsolationLevel(1),
	})
	if err == nil || !strings.Contains(err.Error(), "isolation") {
		t.Fatalf("BeginTx error = %v, want unsupported isolation error", err)
	}
}

func TestConnValidatorRejectsUnavailableNativeState(t *testing.T) {
	for _, tc := range []struct {
		name string
		conn *conn
	}{
		{"missing native", &conn{}},
		{"released native", &conn{native: &nativeConnection{}}},
		{"closed", &conn{closed: true, native: &nativeConnection{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validator, ok := any(tc.conn).(driver.Validator)
			if !ok {
				t.Fatal("connection does not implement driver.Validator")
			}
			if validator.IsValid() {
				t.Fatal("unavailable connection is eligible for pooling")
			}
		})
	}
}

// No attachment is opened: this represents an already released native handle.
type unavailableConnector struct{}

func (unavailableConnector) Connect(context.Context) (driver.Conn, error) {
	return &conn{native: &nativeConnection{}}, nil
}

func (unavailableConnector) Driver() driver.Driver { return driverInstance }

func TestPoolDiscardsUnavailableNativeConnection(t *testing.T) {
	db := sql.OpenDB(unavailableConnector{})
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	connection, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if stats := db.Stats(); stats.OpenConnections != 0 || stats.Idle != 0 {
		t.Fatalf("unavailable connection retained in pool: %+v", stats)
	}
}

func TestConnValidatorConcurrentClose(t *testing.T) {
	c := &conn{native: &nativeConnection{}}
	validator, ok := any(c).(driver.Validator)
	if !ok {
		t.Fatal("connection does not implement driver.Validator")
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			validator.IsValid()
		}
	}()
	go func() {
		defer wg.Done()
		_ = c.Close()
	}()
	wg.Wait()
	if validator.IsValid() {
		t.Fatal("closed connection is eligible for pooling")
	}
}

func TestConnCloseInvalidatesReachablePreparedStatements(t *testing.T) {
	c := &conn{statements: make(map[*stmt]struct{})}
	s := &stmt{conn: c, native: &nativeStatement{}}
	c.statements[s] = struct{}{}

	if err := c.Close(); err != nil {
		t.Fatalf("connection close: %v", err)
	}
	if !s.closed || s.native != nil {
		t.Fatalf("prepared statement remained usable after connection close: closed=%t native=%v", s.closed, s.native)
	}
	if _, err := s.ExecContext(context.Background(), nil); !errors.Is(err, errStatementClosed) {
		t.Fatalf("statement execution after connection close = %v, want closed-statement error", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second statement close: %v", err)
	}
}

func TestStmtCloseIsIdempotent(t *testing.T) {
	c := &conn{statements: make(map[*stmt]struct{})}
	s := &stmt{conn: c, native: &nativeStatement{}}
	c.statements[s] = struct{}{}

	if err := s.Close(); err != nil {
		t.Fatalf("first statement close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second statement close: %v", err)
	}
	if !s.closed || s.native != nil {
		t.Fatalf("statement close did not clear native ownership: closed=%t native=%v", s.closed, s.native)
	}
}

func TestStmtCloseDoesNotCloseDirectSiblingRowsAfterRepeatedClose(t *testing.T) {
	c := &conn{
		rows:       make(map[*rows]struct{}),
		statements: make(map[*stmt]struct{}),
	}
	s := &stmt{conn: c, native: &nativeStatement{}}
	direct := &rows{
		conn:   c,
		native: &nativeCursor{},
	}
	c.statements[s] = struct{}{}
	c.rows[direct] = struct{}{}

	if err := s.Close(); err != nil {
		t.Fatalf("first statement close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second statement close: %v", err)
	}
	if direct.closed {
		t.Fatal("repeated statement close closed an unrelated direct row")
	}
	if _, ok := c.rows[direct]; !ok {
		t.Fatal("repeated statement close unregistered an unrelated direct row")
	}
}

func TestInvalidatedSiblingRowsDoNotReportCleanEOF(t *testing.T) {
	c := &conn{rows: make(map[*rows]struct{})}
	r := &rows{
		conn:   c,
		native: &nativeCursor{},
	}
	c.rows[r] = struct{}{}

	cause := errors.New("query failed while invalidating connection")
	c.invalidateLocked(cause)
	err := r.Next(nil)
	if err == nil || errors.Is(err, io.EOF) || !errors.Is(err, cause) {
		t.Fatalf("invalidated row Next error = %v, want terminal error", err)
	}
	if r.native != nil {
		t.Fatal("invalidated row retained a native cursor pointer")
	}
}

func TestDatabaseSQLPreparedExecContextCancelsActiveNativeCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	closeStarted := make(chan struct{})
	closeOnce := sync.Once{}
	callCount := 0
	nativeErr := &Error{
		Operation:  "execute prepared statement",
		NativeCode: nativeCancelledCode,
		Message:    "isc_cancelled",
	}
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
			prepareOverride: func(string) (*nativeStatement, error) {
				return &nativeStatement{
					numInputOverride: func() int { return -1 },
					execOverride: func([]argument) (int64, error) {
						callCount++
						if callCount == 1 {
							close(entered)
							<-release
							return 0, nativeErr
						}
						return 13, nil
					},
					closeOverride: func() error {
						closeOnce.Do(func() { close(closeStarted) })
						return nil
					},
				}, nil
			},
		},
	}
	db := openDatabaseSQLTestDB(t, connection)

	statement, err := db.PrepareContext(context.Background(), "UPDATE example SET value = 1")
	if err != nil {
		t.Fatalf("DB.PrepareContext() error = %v", err)
	}
	defer statement.Close()

	resultDone := make(chan error, 1)
	go func() {
		result, callErr := statement.ExecContext(ctx)
		if result != nil {
			callErr = errors.Join(callErr, errors.New("canceled prepared execution returned a result"))
		}
		resultDone <- callErr
	}()
	waitForTestSignal(t, entered, "prepared execution did not enter the native call")
	cancel()

	closeDone := make(chan error, 1)
	go func() { closeDone <- statement.Close() }()
	select {
	case <-closeStarted:
		t.Fatal("prepared statement closed before the native worker returned")
	case <-closeDone:
		t.Fatal("prepared statement close returned before the native worker returned")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	if err := <-resultDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("prepared ExecContext() error = %v, want context.Canceled", err)
	} else {
		var gotNative *Error
		if !errors.As(err, &gotNative) || gotNative.NativeCode != nativeCancelledCode {
			t.Fatalf("prepared ExecContext() error = %v, want native cancellation diagnostics", err)
		}
		if errors.Is(err, driver.ErrBadConn) {
			t.Fatal("prepared cancellation was classified as driver.ErrBadConn")
		}
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("prepared statement Close() error = %v", err)
	}
}

func TestDatabaseSQLPreparedExecContextReusesStatementAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	callCount := 0
	nativeErr := &Error{
		Operation:  "execute prepared statement",
		NativeCode: nativeCancelledCode,
		Message:    "isc_cancelled",
	}
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
			prepareOverride: func(string) (*nativeStatement, error) {
				return &nativeStatement{
					numInputOverride: func() int { return -1 },
					execOverride: func([]argument) (int64, error) {
						callCount++
						if callCount == 1 {
							close(entered)
							<-release
							return 0, nativeErr
						}
						return 17, nil
					},
				}, nil
			},
		},
	}
	db := openDatabaseSQLTestDB(t, connection)
	statement, err := db.PrepareContext(context.Background(), "UPDATE example SET value = 1")
	if err != nil {
		t.Fatalf("DB.PrepareContext() error = %v", err)
	}
	defer statement.Close()

	firstDone := make(chan error, 1)
	go func() {
		_, callErr := statement.ExecContext(ctx)
		firstDone <- callErr
	}()
	waitForTestSignal(t, entered, "prepared execution did not enter the native call")
	cancel()
	close(release)
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prepared ExecContext() error = %v, want context.Canceled", err)
	}

	result, err := statement.ExecContext(context.Background())
	if err != nil {
		t.Fatalf("prepared ExecContext() reuse error = %v", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 17 {
		t.Fatalf("prepared reuse rows affected = (%d, %v), want (17, nil)", affected, err)
	}
	if connection.closed {
		t.Fatal("canceled prepared execution invalidated a reusable connection")
	}
}

func TestDatabaseSQLPreparedQueryContextCancelsActiveNativeCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	nativeErr := &Error{
		Operation:  "execute prepared SELECT",
		NativeCode: nativeCancelledCode,
		Message:    "isc_cancelled",
	}
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
			prepareOverride: func(string) (*nativeStatement, error) {
				return &nativeStatement{
					numInputOverride: func() int { return -1 },
					queryOverride: func([]argument) (*nativeCursor, []string, error) {
						close(entered)
						<-release
						return nil, nil, nativeErr
					},
				}, nil
			},
		},
	}
	db := openDatabaseSQLTestDB(t, connection)
	statement, err := db.PrepareContext(context.Background(), "SELECT value FROM example")
	if err != nil {
		t.Fatalf("DB.PrepareContext() error = %v", err)
	}
	defer statement.Close()

	resultDone := make(chan error, 1)
	go func() {
		rows, callErr := statement.QueryContext(ctx)
		if rows != nil {
			callErr = errors.Join(callErr, errors.New("canceled prepared query returned rows"))
		}
		resultDone <- callErr
	}()
	waitForTestSignal(t, entered, "prepared query did not enter the native call")
	cancel()
	select {
	case err := <-resultDone:
		t.Fatalf("prepared QueryContext() returned before the native worker: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	err = <-resultDone
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("prepared QueryContext() error = %v, want context.Canceled", err)
	}
	var gotNative *Error
	if !errors.As(err, &gotNative) || gotNative.NativeCode != nativeCancelledCode {
		t.Fatalf("prepared QueryContext() error = %v, want native cancellation diagnostics", err)
	}
	if errors.Is(err, driver.ErrBadConn) {
		t.Fatal("prepared query cancellation was classified as driver.ErrBadConn")
	}
}

func TestDatabaseSQLPreparedQueryContextPreservesCompletedNativeResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
			prepareOverride: func(string) (*nativeStatement, error) {
				return &nativeStatement{
					numInputOverride: func() int { return -1 },
					queryOverride: func([]argument) (*nativeCursor, []string, error) {
						close(entered)
						<-release
						return &nativeCursor{}, nil, nil
					},
				}, nil
			},
		},
	}
	db := openDatabaseSQLTestDB(t, connection)
	statement, err := db.PrepareContext(context.Background(), "SELECT value FROM example")
	if err != nil {
		t.Fatalf("DB.PrepareContext() error = %v", err)
	}
	defer statement.Close()

	type queryResult struct {
		rows *sql.Rows
		err  error
	}
	resultDone := make(chan queryResult, 1)
	go func() {
		rows, queryErr := statement.QueryContext(ctx)
		resultDone <- queryResult{rows: rows, err: queryErr}
	}()
	waitForTestSignal(t, entered, "prepared query did not enter the native call")
	cancel()
	close(release)
	result := <-resultDone
	if result.err != nil {
		t.Fatalf("completed prepared QueryContext() error = %v, want nil", result.err)
	}
	if result.rows == nil {
		t.Fatal("completed prepared QueryContext() returned nil rows")
	}
	if err := result.rows.Close(); err != nil {
		t.Fatalf("completed prepared query Rows.Close() error = %v", err)
	}
}

func TestRowsNextCancelsActiveNativeFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	abortStarted := make(chan struct{})
	nativeErr := &Error{
		Operation:  "fetch row",
		NativeCode: nativeCancelledCode,
		Message:    "isc_cancelled",
	}
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
		},
	}
	native := &nativeCursor{
		nextOverride: func() (bool, error) {
			close(entered)
			<-release
			return false, nativeErr
		},
		abortOverride: func() error {
			close(abortStarted)
			return nil
		},
	}
	result := &rows{conn: connection, native: native, ctx: ctx}
	nextDone := make(chan error, 1)
	go func() { nextDone <- result.Next(nil) }()
	waitForTestSignal(t, entered, "rows fetch did not enter the native call")
	cancel()
	select {
	case <-abortStarted:
		t.Fatal("rows aborted the cursor before the native fetch returned")
	case err := <-nextDone:
		t.Fatalf("rows.Next() returned before the native fetch returned: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	err := <-nextDone
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("rows.Next() error = %v, want context.Canceled", err)
	}
	var gotNative *Error
	if !errors.As(err, &gotNative) || gotNative.NativeCode != nativeCancelledCode {
		t.Fatalf("rows.Next() error = %v, want native cancellation diagnostics", err)
	}
	if errors.Is(err, driver.ErrBadConn) {
		t.Fatal("rows fetch cancellation was classified as driver.ErrBadConn")
	}
}

func TestRowsNextPreservesCompletedNativeFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
		},
	}
	native := &nativeCursor{
		nextOverride: func() (bool, error) {
			close(entered)
			<-release
			return true, nil
		},
		abortOverride: func() error { return nil },
	}
	result := &rows{conn: connection, native: native, ctx: ctx}
	nextDone := make(chan error, 1)
	go func() { nextDone <- result.Next(nil) }()
	waitForTestSignal(t, entered, "rows fetch did not enter the native call")
	cancel()
	close(release)
	if err := <-nextDone; err != nil {
		t.Fatalf("completed rows.Next() error = %v, want nil", err)
	}
	if result.closed {
		t.Fatal("completed rows.Next() aborted the successful row")
	}
	if err := result.Close(); err != nil {
		t.Fatalf("completed rows.Close() error = %v", err)
	}
}

var (
	_ driver.Connector = (*connector)(nil)
)
