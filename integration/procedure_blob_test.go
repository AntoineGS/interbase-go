//go:build integration

package integration_test

import (
	"database/sql"
	"io"
	"testing"

	interbase "interbase-go"
)

// TestSelectableProcedureTextBlobOutputs reads text BLOB outputs of
// selectable procedures through every query path. The WIN1250 output only
// reads back as the expected UTF-8 text when the driver uses the declared
// output character set from the procedure catalog, so a guessed or missing
// character set cannot pass.
func TestSelectableProcedureTextBlobOutputs(t *testing.T) {
	const want = "ě Ž"
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	// As in TestDirectBlobCharsetMetadataForExpressionsAndProcedures, the
	// WIN1250 procedure is defined through a WIN1250 attachment so its
	// returned bytes match its declared character set.
	win1250DB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"WIN1250", 3, interbase.TransactionOptions{})
	ctx := readContext(t)
	procedures := []struct {
		db         *sql.DB
		name       string
		definition string
	}{
		{db: db, name: "LP_SEL_BLOB_UTF8", definition: `CREATE PROCEDURE LP_SEL_BLOB_UTF8
RETURNS (ID INTEGER, B BLOB SUB_TYPE TEXT CHARACTER SET UTF8)
AS
BEGIN
  ID = 1;
  B = 'ě Ž';
  SUSPEND;
  ID = 2;
  SUSPEND;
END`},
		{db: win1250DB, name: "LP_SEL_BLOB_WIN1250", definition: `CREATE PROCEDURE LP_SEL_BLOB_WIN1250
RETURNS (ID INTEGER, B BLOB SUB_TYPE TEXT CHARACTER SET WIN1250)
AS
BEGIN
  ID = 1;
  B = 'ě Ž';
  SUSPEND;
  ID = 2;
  SUSPEND;
END`},
	}
	for _, procedure := range procedures {
		if _, err := procedure.db.ExecContext(ctx, procedure.definition); err != nil {
			t.Fatalf("create %s: %v", procedure.name, err)
		}
	}

	for _, charset := range []string{"UTF8", "NONE"} {
		t.Run("direct "+charset, func(t *testing.T) {
			attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
				"", charset, 3, interbase.TransactionOptions{})
			tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{ReadOnly: true})
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback()
			readRows := func(label string, cursor *interbase.Cursor, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: query: %v", label, err)
				}
				defer cursor.Close()
				rows := 0
				for {
					hasRow, err := cursor.Next(ctx)
					if err != nil {
						t.Fatalf("%s: Next(): %v", label, err)
					}
					if !hasRow {
						break
					}
					row, err := cursor.Row()
					if err != nil {
						t.Fatalf("%s: Row(): %v", label, err)
					}
					rows++
					ref, ok := row[1].Value.(interbase.BlobRef)
					if !ok {
						t.Fatalf("%s: row %d value type = %T, want interbase.BlobRef", label, rows, row[1].Value)
					}
					reader, err := tx.OpenBlob(ctx, ref)
					if err != nil {
						t.Fatalf("%s: open BLOB: %v", label, err)
					}
					data, err := io.ReadAll(reader)
					closeErr := reader.Close()
					if err != nil || closeErr != nil {
						t.Fatalf("%s: read BLOB = %v, close = %v", label, err, closeErr)
					}
					if string(data) != want {
						t.Fatalf("%s: row %d BLOB = %q (%x), want %q", label, rows, data, data, want)
					}
				}
				if rows != 2 {
					t.Fatalf("%s: rows = %d, want 2", label, rows)
				}
			}
			for _, procedure := range procedures {
				query := "SELECT ID, B FROM " + procedure.name
				cursor, err := tx.Query(ctx, query)
				readRows(procedure.name+" Query", cursor, err)
				statement, err := tx.Prepare(ctx, query)
				if err != nil {
					t.Fatalf("%s: prepare: %v", procedure.name, err)
				}
				for execution := 1; execution <= 2; execution++ {
					cursor, err := statement.Query(ctx)
					readRows(procedure.name+" prepared", cursor, err)
				}
				if err := statement.Close(); err != nil {
					t.Fatalf("%s: close prepared: %v", procedure.name, err)
				}
			}
		})
		t.Run("database/sql "+charset, func(t *testing.T) {
			pool := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, charset, 3,
				interbase.TransactionOptions{})
			for _, procedure := range procedures {
				rows, err := pool.QueryContext(ctx, "SELECT ID, B FROM "+procedure.name)
				if err != nil {
					t.Fatalf("%s: query: %v", procedure.name, err)
				}
				count := 0
				for rows.Next() {
					var id int64
					var text string
					if err := rows.Scan(&id, &text); err != nil {
						_ = rows.Close()
						t.Fatalf("%s: scan: %v", procedure.name, err)
					}
					count++
					if text != want {
						_ = rows.Close()
						t.Fatalf("%s: row %d BLOB = %q, want %q", procedure.name, count, text, want)
					}
				}
				if err := rows.Err(); err != nil {
					t.Fatalf("%s: rows: %v", procedure.name, err)
				}
				_ = rows.Close()
				if count != 2 {
					t.Fatalf("%s: rows = %d, want 2", procedure.name, count)
				}
			}
		})
	}
}
