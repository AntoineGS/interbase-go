package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
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
		})
	}
}

func TestConvertArgumentRejectsTypesOutsidePoCBoundary(t *testing.T) {
	for _, input := range []any{
		int(1),
		float32(1),
		[]byte("not supported"),
		struct{}{},
	} {
		if _, err := convertArgument(input); err == nil {
			t.Fatalf("convertArgument(%T) returned nil error", input)
		}
	}
}

func TestConvertArgumentRejectsOversizedString(t *testing.T) {
	if _, err := convertArgument(strings.Repeat("x", math.MaxInt16+1)); err == nil {
		t.Fatal("convertArgument accepted a string larger than the SQLDA limit")
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

func TestConnRejectsStateChangingOperations(t *testing.T) {
	conn := new(conn)
	ctx := context.Background()

	if _, err := conn.Prepare("SELECT 1"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Prepare error = %v, want explicit unsupported error", err)
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM secret", nil); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("ExecContext error = %v, want explicit unsupported error", err)
	}
	if _, err := conn.Begin(); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Begin error = %v, want explicit unsupported error", err)
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

var (
	_ driver.Connector = (*connector)(nil)
)
