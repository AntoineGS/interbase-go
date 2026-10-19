//go:build integration

package integration_test

import (
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	interbase "interbase-go"
	"interbase-go/internal/testfixture"
)

func TestDirectPreparedStatementReuse(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)

	if _, err := db.ExecContext(ctx, "CREATE TABLE LP_T (ID INTEGER, V VARCHAR(10))"); err != nil {
		t.Fatalf("create LP_T: %v", err)
	}
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	insert, err := tx.Prepare(ctx, "INSERT INTO LP_T (ID, V) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("prepare insert: %v", err)
	}
	inputs := insert.Inputs()
	if len(inputs) != 2 || inputs[0].Kind != "INTEGER" || inputs[1].Kind != "VARCHAR" {
		t.Fatalf("insert inputs = %#v, want INTEGER, VARCHAR", inputs)
	}
	if len(insert.Outputs()) != 0 {
		t.Fatalf("insert outputs = %#v, want none", insert.Outputs())
	}
	values := []string{"one", "two", "three"}
	for index, value := range values {
		affected, err := insert.Exec(ctx, int64(index+1), value)
		if err != nil || affected != 1 {
			t.Fatalf("insert %d = (%d, %v), want (1, nil)", index+1, affected, err)
		}
	}

	selectStatement, err := tx.Prepare(ctx, "SELECT V FROM LP_T WHERE ID = ?")
	if err != nil {
		t.Fatalf("prepare select: %v", err)
	}
	outputs := selectStatement.Outputs()
	// Length is the SQLDA byte length; the fixture's UTF8 default takes four
	// bytes per character (character set id 59).
	wantLength := 10
	if len(outputs) == 1 && outputs[0].SubType == 59 {
		wantLength = 40
	}
	if len(outputs) != 1 || outputs[0].Name != "V" || outputs[0].Kind != "VARCHAR" ||
		outputs[0].Length != wantLength || !outputs[0].Nullable {
		t.Fatalf("select outputs = %#v, want nullable VARCHAR V of length %d", outputs, wantLength)
	}
	if inputs := selectStatement.Inputs(); len(inputs) != 1 || inputs[0].Kind != "INTEGER" {
		t.Fatalf("select inputs = %#v, want INTEGER", inputs)
	}
	for index, want := range values {
		cursor, err := selectStatement.Query(ctx, int64(index+1))
		if err != nil {
			t.Fatalf("query %d: %v", index+1, err)
		}
		if _, err := selectStatement.Query(ctx, int64(1)); err == nil {
			t.Fatal("second Query with an open cursor succeeded")
		}
		hasRow, err := cursor.Next(ctx)
		if err != nil || !hasRow {
			t.Fatalf("Next %d = (%v, %v), want row", index+1, hasRow, err)
		}
		row, err := cursor.Row()
		if err != nil || len(row) != 1 || row[0].Value != want {
			t.Fatalf("row %d = %#v, %v, want %q", index+1, row, err, want)
		}
		if err := cursor.Close(); err != nil {
			t.Fatalf("close cursor %d: %v", index+1, err)
		}
	}
	if _, err := selectStatement.Exec(ctx, int64(1)); err == nil ||
		!strings.Contains(err.Error(), "statement produces a result set; use Query instead") {
		t.Fatalf("Exec of a SELECT statement error = %v, want the use-Query error", err)
	}
	if cursor, err := selectStatement.Query(ctx, int64(1)); err != nil {
		t.Fatalf("Query after rejected Exec: %v", err)
	} else if err := cursor.Close(); err != nil {
		t.Fatalf("close cursor after rejected Exec: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := selectStatement.Query(ctx, int64(1)); err == nil {
		t.Fatal("Query after rollback succeeded")
	}
	if _, err := insert.Exec(ctx, int64(9), "nine"); err == nil {
		t.Fatal("Exec after rollback succeeded")
	}
	if err := insert.Close(); err != nil {
		t.Fatalf("close after rollback: %v", err)
	}
	if err := insert.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// lpCatalogFixture creates a UTF8 CHAR(5) column, a text BLOB and a
// procedure returning that BLOB.
func lpCatalogFixture(t *testing.T) (*testfixture.Database, testfixture.Config, *fixtureCleanup) {
	t.Helper()
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	ctx := readContext(t)
	for _, statement := range []string{
		"CREATE TABLE LP_CAT (ID INTEGER, C5 CHAR(5) CHARACTER SET UTF8, B BLOB SUB_TYPE TEXT CHARACTER SET UTF8)",
		"INSERT INTO LP_CAT (ID, C5, B) VALUES (1, 'ab', 'hello blob')",
		`CREATE PROCEDURE LP_CAT_BLOB RETURNS (B BLOB SUB_TYPE TEXT CHARACTER SET UTF8) AS
BEGIN
  SELECT B FROM LP_CAT WHERE ID = 1 INTO :B;
END`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(statement, "\n", 2)[0], err)
		}
	}
	return fixture, cfg, cleanup
}

func lpFirstRow(t *testing.T, ctx context.Context, cursor *interbase.Cursor, err error) []interbase.Cell {
	t.Helper()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer cursor.Close()
	hasRow, err := cursor.Next(ctx)
	if err != nil || !hasRow {
		t.Fatalf("Next() = %v, %v", hasRow, err)
	}
	row, err := cursor.Row()
	if err != nil {
		t.Fatalf("Row(): %v", err)
	}
	return row
}

func TestDirectPreparedFixedTextMatchesQuery(t *testing.T) {
	fixture, cfg, cleanup := lpCatalogFixture(t)
	ctx := readContext(t)
	for _, charset := range []string{"", "NONE"} {
		t.Run("charset "+charset, func(t *testing.T) {
			attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
				"", charset, 3, interbase.TransactionOptions{})
			tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{ReadOnly: true})
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback()
			const query = "SELECT C5 FROM LP_CAT WHERE ID = 1"
			cursor, err := tx.Query(ctx, query)
			want := lpFirstRow(t, ctx, cursor, err)[0].Value
			statement, err := tx.Prepare(ctx, query)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			for execution := 1; execution <= 2; execution++ {
				cursor, err := statement.Query(ctx)
				got := lpFirstRow(t, ctx, cursor, err)[0].Value
				if got != want {
					t.Fatalf("execution %d: prepared value %q, Query value %q", execution, got, want)
				}
			}
			if charset == "" && want != "ab   " {
				t.Fatalf("Query value %q, want CHAR(5) %q", want, "ab   ")
			}
		})
	}
}

func TestDirectPreparedProcedureTextBlob(t *testing.T) {
	fixture, cfg, cleanup := lpCatalogFixture(t)
	ctx := readContext(t)
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(ctx, "EXECUTE PROCEDURE LP_CAT_BLOB")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for execution := 1; execution <= 2; execution++ {
		cursor, err := statement.Query(ctx)
		cell := lpFirstRow(t, ctx, cursor, err)[0]
		var text string
		switch value := cell.Value.(type) {
		case string:
			text = value
		case interbase.BlobRef:
			reader, err := tx.OpenBlob(ctx, value)
			if err != nil {
				t.Fatalf("open BLOB: %v", err)
			}
			data, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				t.Fatalf("read BLOB: %v", err)
			}
			text = string(data)
		default:
			t.Fatalf("BLOB output value %#v (%T)", cell.Value, cell.Value)
		}
		if text != "hello blob" {
			t.Fatalf("execution %d: BLOB output %q", execution, text)
		}
	}
}

// lpCatalogIdxReads returns the attachment's cumulative index reads of
// RDB$RELATION_FIELDS (relation 5).
func lpCatalogIdxReads(t *testing.T, ctx context.Context, attachment *interbase.Attachment) int64 {
	t.Helper()
	item, err := attachment.DatabaseInfo(ctx, 24)
	if err != nil {
		t.Fatalf("index read info: %v", err)
	}
	data := item.Bytes()
	var total int64
	for offset := 0; offset+6 <= len(data); offset += 6 {
		if binary.LittleEndian.Uint16(data[offset:]) == 5 {
			total += int64(binary.LittleEndian.Uint32(data[offset+2:]))
		}
	}
	return total
}

func TestDirectPreparedCatalogQueryRunsOnce(t *testing.T) {
	fixture, cfg, cleanup := lpCatalogFixture(t)
	ctx := readContext(t)
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	const query = "SELECT ID, C5 FROM LP_CAT WHERE ID = 1"
	// Load the relation's metadata first, so later catalog reads are the
	// driver's column-type query only.
	cursor, err := tx.Query(ctx, query)
	lpFirstRow(t, ctx, cursor, err)

	before := lpCatalogIdxReads(t, ctx, attachment)
	statement, err := tx.Prepare(ctx, query)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	afterPrepare := lpCatalogIdxReads(t, ctx, attachment)
	if afterPrepare == before {
		t.Fatal("prepare did not run the column-type catalog query")
	}
	for execution := 1; execution <= 3; execution++ {
		start := lpCatalogIdxReads(t, ctx, attachment)
		cursor, err := statement.Query(ctx)
		lpFirstRow(t, ctx, cursor, err)
		if delta := lpCatalogIdxReads(t, ctx, attachment) - start; delta != 0 {
			t.Fatalf("execution %d read RDB$RELATION_FIELDS %d times; the catalog query must run once, at prepare", execution, delta)
		}
	}
}
