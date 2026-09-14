package interbase_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	interbase "interbase-go"
)

func liveDB(t *testing.T) *sql.DB {
	t.Helper()
	database := os.Getenv("INTERBASE_DATABASE")
	if database == "" {
		t.Skip("set INTERBASE_DATABASE, INTERBASE_USER, INTERBASE_PASSWORD to run native live tests")
	}
	password, ok := os.LookupEnv("INTERBASE_PASSWORD")
	if !ok || os.Getenv("INTERBASE_USER") == "" {
		t.Fatal("live connection settings are incomplete")
	}
	connector, err := interbase.NewConnector(interbase.Config{
		Database: database, User: os.Getenv("INTERBASE_USER"), Password: password,
		Dialect: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	return db
}

func TestLiveDialectOneScalars(t *testing.T) {
	db := liveDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var text, padded string
	var number int64
	var nullable sql.NullString
	var timestamp time.Time
	err := db.QueryRowContext(ctx, `SELECT "dialect one", CAST('x' AS CHAR(3)),
 CAST(-42 AS INTEGER), CAST(NULL AS VARCHAR(10)),
 CAST('2024-02-29 12:34:56.1234' AS TIMESTAMP) FROM RDB$DATABASE`).
		Scan(&text, &padded, &number, &nullable, &timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if text != "dialect one" || padded != "x  " || number != -42 || nullable.Valid {
		t.Fatal("unexpected Dialect 1 scalar, padding, or NULL conversion")
	}
	want := time.Date(2024, 2, 29, 12, 34, 56, 123400000, time.UTC)
	if !timestamp.Equal(want) {
		t.Fatalf("timestamp = %v, want %v", timestamp, want)
	}
}

func TestLivePositionalParameters(t *testing.T) {
	db := liveDB(t)
	tests := []struct {
		name, cast string
		input      any
		want       any
	}{
		{"string", "VARCHAR(40)", "quote ' and \" with spaces  ", "quote ' and \" with spaces  "},
		{"unicode", "VARCHAR(40) CHARACTER SET UTF8", "caf\u00e9", "caf\u00e9"},
		{"empty", "VARCHAR(40)", "", ""},
		{"integer", "INTEGER", int64(-42), int64(-42)},
		{"float", "DOUBLE PRECISION", float64(1.25), float64(1.25)},
		{"null", "VARCHAR(40)", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got any
			if err := db.QueryRow("SELECT CAST(? AS "+tt.cast+") FROM RDB$DATABASE", tt.input).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("parameter round trip = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestLiveCatalogAndCursorReuse(t *testing.T) {
	db := liveDB(t)
	for i := 0; i < 20; i++ {
		rows, err := db.Query("SELECT RDB$RELATION_NAME FROM RDB$RELATIONS")
		if err != nil {
			t.Fatal(err)
		}
		if !rows.Next() {
			err := rows.Err()
			rows.Close()
			t.Fatalf("system catalog returned no row: %v", err)
		}
		// Closing before EOF must release the native cursor and its transaction.
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var relation string
	if err := db.QueryRow("SELECT RDB$RELATION_NAME FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = ?", "RDB$DATABASE").Scan(&relation); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(relation) != "RDB$DATABASE" {
		t.Fatal("catalog parameter did not select the expected system relation")
	}
	if stats := db.Stats(); stats.OpenConnections != 1 || stats.InUse != 0 {
		t.Fatalf("unexpected connection reuse: %+v", stats)
	}
}

func TestLiveRecoveryAfterErrors(t *testing.T) {
	db := liveDB(t)
	for _, test := range []struct {
		query string
		args  []any
	}{
		{"SELECT FROM", nil},
		{"SELECT CAST(? AS INTEGER) FROM RDB$DATABASE", nil},
		{"SELECT RDB$DESCRIPTION FROM RDB$DATABASE", nil}, // BLOBs intentionally unsupported.
	} {
		rows, err := db.Query(test.query, test.args...)
		if err == nil {
			rows.Close()
			t.Fatal("unsupported or invalid query unexpectedly succeeded")
		}
		if err := db.Ping(); err != nil {
			t.Fatalf("connection unusable after query error: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	rows, err := db.QueryContext(ctx, "SELECT RDB$RELATION_NAME FROM RDB$RELATIONS")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if rows.Next() || !errors.Is(rows.Err(), context.Canceled) {
		rows.Close()
		t.Fatalf("cancelled cursor error = %v", rows.Err())
	}
	rows.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("connection unusable after cancellation: %v", err)
	}
}

func TestLiveUDFMetadata(t *testing.T) {
	db := liveDB(t)
	rows, err := db.Query(`SELECT f.RDB$FUNCTION_NAME, f.RDB$RETURN_ARGUMENT,
 a.RDB$ARGUMENT_POSITION, a.RDB$FIELD_TYPE, a.RDB$FIELD_SCALE,
 a.RDB$FIELD_LENGTH, a.RDB$MECHANISM
 FROM RDB$FUNCTIONS f JOIN RDB$FUNCTION_ARGUMENTS a
 ON a.RDB$FUNCTION_NAME = f.RDB$FUNCTION_NAME
 ORDER BY f.RDB$FUNCTION_NAME, a.RDB$ARGUMENT_POSITION`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name string
		var ret, pos, typ, scale, length, mechanism sql.NullInt64
		if err := rows.Scan(&name, &ret, &pos, &typ, &scale, &length, &mechanism); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(name) == "" || !pos.Valid || !typ.Valid {
			t.Fatal("UDF metadata is missing its name, argument position, or type")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Log("catalog query succeeded; no UDF arguments are declared in this database")
	} else {
		t.Logf("decoded %d UDF argument catalog rows; no UDFs were invoked", count)
	}
}
