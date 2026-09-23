//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	interbase "interbase-go"
	catalogschema "interbase-go/schema"
)

func TestDialect1GeneratedDDLReplay(t *testing.T) {
	ctx := readContext(t)
	sourceFixture, sourceConfig, sourceCleanup := createFixture(t, 1, "")
	source := openDatabase(t, sourceCleanup, sourceFixture.ConnectionString(), sourceConfig.User, sourceConfig.Password, "", 1, interbase.TransactionOptions{})
	replayFixture, replayConfig, replayCleanup := createFixture(t, 1, "")
	replay := openDatabase(t, replayCleanup, replayFixture.ConnectionString(), replayConfig.User, replayConfig.Password, "", 1, interbase.TransactionOptions{})

	for _, statement := range []string{
		"CREATE DOMAIN GO_REPLAY_AMOUNT AS NUMERIC(12,2) DEFAULT 1.25 NOT NULL CHECK (VALUE >= 0)",
		"CREATE TABLE GO_REPLAY_TABLE (ID INTEGER NOT NULL, AMOUNT GO_REPLAY_AMOUNT DEFAULT 2.50, CONSTRAINT GO_REPLAY_PK PRIMARY KEY (ID), CONSTRAINT GO_REPLAY_AMOUNT_CK CHECK (AMOUNT >= 0))",
	} {
		if _, err := source.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create source object %q: %v", statement, err)
		}
	}

	sourceCatalog := catalogschema.New(source)
	domain, err := sourceCatalog.Domain(ctx, "GO_REPLAY_AMOUNT")
	if err != nil || domain == nil {
		t.Fatalf("load source domain: domain=%#v err=%v", domain, err)
	}
	domainDDL, err := domain.GenerateDDLWithOptions(catalogschema.DDLOptions{Dialect: catalogschema.Dialect1})
	if err != nil {
		t.Fatalf("generate source domain DDL: %v", err)
	}
	table, err := sourceCatalog.Table(ctx, "GO_REPLAY_TABLE")
	if err != nil || table == nil {
		t.Fatalf("load source table: table=%#v err=%v", table, err)
	}
	tableDDL, err := table.GenerateDDLWithOptions(catalogschema.DDLOptions{Dialect: catalogschema.Dialect1})
	if err != nil {
		t.Fatalf("generate source table DDL: %v", err)
	}

	for _, statement := range []string{domainDDL, tableDDL} {
		if _, err := replay.ExecContext(ctx, statement); err != nil {
			t.Fatalf("replay generated DDL %q: %v", statement, err)
		}
	}
	if _, err := source.ExecContext(ctx, "INSERT INTO GO_REPLAY_TABLE (ID, AMOUNT) VALUES (1, 3.75)"); err != nil {
		t.Fatalf("insert source representative value: %v", err)
	}
	if _, err := replay.ExecContext(ctx, "INSERT INTO GO_REPLAY_TABLE (ID, AMOUNT) VALUES (1, 3.75)"); err != nil {
		t.Fatalf("insert replay representative value: %v", err)
	}

	for _, tableName := range []string{"GO_REPLAY_TABLE"} {
		sourceValues := readDialect1FieldTuple(t, ctx, source, tableName, "AMOUNT")
		replayValues := readDialect1FieldTuple(t, ctx, replay, tableName, "AMOUNT")
		if sourceValues != replayValues {
			t.Fatalf("field tuple source=%v replay=%v", sourceValues, replayValues)
		}
	}
	var sourceValue, replayValue float64
	if err := source.QueryRowContext(ctx, "SELECT AMOUNT FROM GO_REPLAY_TABLE WHERE ID=1").Scan(&sourceValue); err != nil {
		t.Fatal(err)
	}
	if err := replay.QueryRowContext(ctx, "SELECT AMOUNT FROM GO_REPLAY_TABLE WHERE ID=1").Scan(&replayValue); err != nil {
		t.Fatal(err)
	}
	if sourceValue != replayValue || sourceValue != 3.75 {
		t.Fatalf("source/replay value = %v/%v, want identical 3.75", sourceValue, replayValue)
	}

	var sourceDefault, replayDefault string
	if err := source.QueryRowContext(ctx, "SELECT RDB$DEFAULT_SOURCE FROM RDB$FIELDS WHERE RDB$FIELD_NAME='GO_REPLAY_AMOUNT'").Scan(&sourceDefault); err != nil {
		t.Fatal(err)
	}
	if err := replay.QueryRowContext(ctx, "SELECT RDB$DEFAULT_SOURCE FROM RDB$FIELDS WHERE RDB$FIELD_NAME='GO_REPLAY_AMOUNT'").Scan(&replayDefault); err != nil {
		t.Fatal(err)
	}
	if sourceDefault != replayDefault {
		t.Fatalf("domain default source differs: %q vs %q", sourceDefault, replayDefault)
	}
}

func readDialect1FieldTuple(t *testing.T, ctx context.Context, db *sql.DB, relation, field string) [5]string {
	t.Helper()
	query := fmt.Sprintf(`SELECT f.RDB$FIELD_TYPE, f.RDB$FIELD_LENGTH, f.RDB$FIELD_SCALE, f.RDB$FIELD_SUB_TYPE, f.RDB$FIELD_PRECISION FROM RDB$RELATION_FIELDS rf JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME=rf.RDB$FIELD_SOURCE WHERE rf.RDB$RELATION_NAME='%s' AND rf.RDB$FIELD_NAME='%s'`, relation, field)
	var values [5]sql.NullInt64
	if err := db.QueryRowContext(ctx, query).Scan(&values[0], &values[1], &values[2], &values[3], &values[4]); err != nil {
		t.Fatalf("read catalog field tuple: %v", err)
	}
	var result [5]string
	for i, value := range values {
		if value.Valid {
			result[i] = fmt.Sprint(value.Int64)
		} else {
			result[i] = "NULL"
		}
	}
	return result
}
