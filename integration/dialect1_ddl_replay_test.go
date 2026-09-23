//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

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

func TestDialect1DateNamesAndLongIdentifiersReplay(t *testing.T) {
	ctx := readContext(t)
	sourceFixture, sourceConfig, sourceCleanup := createFixture(t, 1, "")
	source := openDatabase(t, sourceCleanup, sourceFixture.ConnectionString(), sourceConfig.User, sourceConfig.Password, "", 1, interbase.TransactionOptions{})
	replayFixture, replayConfig, replayCleanup := createFixture(t, 1, "")
	replay := openDatabase(t, replayCleanup, replayFixture.ConnectionString(), replayConfig.User, replayConfig.Password, "", 1, interbase.TransactionOptions{})

	longTable := "GO_DIALECT1_IDENTIFIER_" + strings.Repeat("A", 24)
	longColumn := "GO_DIALECT1_COLUMN_" + strings.Repeat("B", 24)
	if _, err := source.ExecContext(ctx, "CREATE TABLE "+longTable+" ("+longColumn+" INTEGER)"); err != nil {
		t.Fatalf("fixture rejected >31-byte Dialect 1 identifiers (%d/%d): %v", len(longTable), len(longColumn), err)
	}
	maxTable := "T" + strings.Repeat("X", 66)
	maxColumn := "C" + strings.Repeat("Y", 66)
	if _, err := source.ExecContext(ctx, "CREATE TABLE "+maxTable+" ("+maxColumn+" INTEGER)"); err != nil {
		t.Fatalf("67-byte identifier rejected: %v", err)
	}
	tooLongTable := "T" + strings.Repeat("Z", 67)
	if _, err := source.ExecContext(ctx, "CREATE TABLE "+tooLongTable+" (X INTEGER)"); err == nil {
		t.Fatal("68-byte identifier accepted; expected 67-byte catalog limit")
	}
	if _, err := source.ExecContext(ctx, `CREATE TABLE GO_DIALECT1_TEMPORAL (DATE_VALUE DATE DEFAULT '2025-06-07 08:09:10', TIMESTAMP_VALUE TIMESTAMP, AMOUNT NUMERIC(18,2))`); err != nil {
		t.Fatalf("create Dialect 1 DATE/TIMESTAMP mapping fixture: %v", err)
	}
	var dateType, timestampType int64
	if err := source.QueryRowContext(ctx, `SELECT f.RDB$FIELD_TYPE FROM RDB$RELATION_FIELDS rf JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME=rf.RDB$FIELD_SOURCE WHERE rf.RDB$RELATION_NAME='GO_DIALECT1_TEMPORAL' AND rf.RDB$FIELD_NAME='DATE_VALUE'`).Scan(&dateType); err != nil {
		t.Fatal(err)
	}
	if err := source.QueryRowContext(ctx, `SELECT f.RDB$FIELD_TYPE FROM RDB$RELATION_FIELDS rf JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME=rf.RDB$FIELD_SOURCE WHERE rf.RDB$RELATION_NAME='GO_DIALECT1_TEMPORAL' AND rf.RDB$FIELD_NAME='TIMESTAMP_VALUE'`).Scan(&timestampType); err != nil {
		t.Fatal(err)
	}
	if dateType != 35 || timestampType != 35 {
		t.Fatalf("Dialect 1 DATE/TIMESTAMP RDB$FIELD_TYPE=%d/%d, want legacy timestamp field type 35", dateType, timestampType)
	}
	numericSourceTuple := readDialect1FieldTuple(t, ctx, source, "GO_DIALECT1_TEMPORAL", "AMOUNT")
	if numericSourceTuple != [5]string{"27", "8", "-2", "NULL", "NULL"} {
		t.Fatalf("Dialect 1 NUMERIC(18,2) source tuple=%v", numericSourceTuple)
	}

	timestampValue := time.Date(2025, 6, 8, 19, 20, 21, 0, time.UTC)
	if _, err := source.ExecContext(ctx, `INSERT INTO GO_DIALECT1_TEMPORAL (TIMESTAMP_VALUE, AMOUNT) VALUES (?, ?)`, timestampValue, 3.75); err != nil {
		t.Fatalf("insert non-midnight temporal values: %v", err)
	}
	table, err := catalogschema.New(source).Table(ctx, "GO_DIALECT1_TEMPORAL")
	if err != nil || table == nil {
		t.Fatalf("load temporal table: %#v, %v", table, err)
	}
	ddl, err := table.GenerateDDLWithOptions(catalogschema.DDLOptions{Dialect: catalogschema.Dialect1})
	if err != nil {
		t.Fatalf("generate temporal DDL: %v", err)
	}
	if strings.Contains(ddl, "TIMESTAMP_VALUE TIMESTAMP") || !strings.Contains(ddl, "DATE_VALUE DATE DEFAULT '2025-06-07 08:09:10'") || !strings.Contains(ddl, "TIMESTAMP_VALUE DATE") || !strings.Contains(ddl, "AMOUNT NUMERIC(15, 2)") {
		t.Fatalf("Dialect 1 temporal DDL must use DATE spelling for field type 35:\n%s", ddl)
	}
	if _, err := replay.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("replay temporal DDL: %v\n%s", err, ddl)
	}
	numericReplayTuple := readDialect1FieldTuple(t, ctx, replay, "GO_DIALECT1_TEMPORAL", "AMOUNT")
	if numericSourceTuple != numericReplayTuple {
		t.Fatalf("Dialect 1 NUMERIC(18,2) source/replay tuples=%v/%v", numericSourceTuple, numericReplayTuple)
	}
	if _, err := replay.ExecContext(ctx, `INSERT INTO GO_DIALECT1_TEMPORAL (TIMESTAMP_VALUE, AMOUNT) VALUES (?, ?)`, timestampValue, 3.75); err != nil {
		t.Fatalf("insert replay temporal values: %v", err)
	}
	var sourceDate, replayDate, sourceTimestamp, replayTimestamp string
	if err := source.QueryRowContext(ctx, `SELECT CAST(DATE_VALUE AS VARCHAR(24)), CAST(TIMESTAMP_VALUE AS VARCHAR(24)) FROM GO_DIALECT1_TEMPORAL`).Scan(&sourceDate, &sourceTimestamp); err != nil {
		t.Fatal(err)
	}
	if err := replay.QueryRowContext(ctx, `SELECT CAST(DATE_VALUE AS VARCHAR(24)), CAST(TIMESTAMP_VALUE AS VARCHAR(24)) FROM GO_DIALECT1_TEMPORAL`).Scan(&replayDate, &replayTimestamp); err != nil {
		t.Fatal(err)
	}
	if sourceDate != replayDate || sourceTimestamp != replayTimestamp || !strings.Contains(sourceDate, "8:09:10") || !strings.Contains(sourceTimestamp, "19:20:21") {
		t.Fatalf("source/replay timestamp text = (%q,%q)/(%q,%q)", sourceDate, sourceTimestamp, replayDate, replayTimestamp)
	}
}

func TestDialect1ProcedureSourceReplaysVerbatim(t *testing.T) {
	ctx := readContext(t)
	sourceFixture, sourceConfig, sourceCleanup := createFixture(t, 1, "")
	source := openDatabase(t, sourceCleanup, sourceFixture.ConnectionString(), sourceConfig.User, sourceConfig.Password, "", 1, interbase.TransactionOptions{})
	replayFixture, replayConfig, replayCleanup := createFixture(t, 1, "")
	replay := openDatabase(t, replayCleanup, replayFixture.ConnectionString(), replayConfig.User, replayConfig.Password, "", 1, interbase.TransactionOptions{})
	const sourceSQL = `CREATE PROCEDURE GO_DIALECT1_SOURCE AS BEGIN POST_EVENT "quoted literal"; END`
	if _, err := source.ExecContext(ctx, sourceSQL); err != nil {
		t.Fatalf("create same-Dialect 1 procedure: %v", err)
	}
	procedure, err := catalogschema.New(source).Procedure(ctx, "GO_DIALECT1_SOURCE")
	if err != nil || procedure == nil {
		t.Fatalf("load procedure: %#v, %v", procedure, err)
	}
	// InterBase 15 stores NULL parameter counts for a parameterless procedure.
	// The fixture's CREATE text proves these are zero; provide that known shape
	// for the DDL call without relaxing production validation of NULL counts.
	if len(procedure.InputParameters) != 0 || len(procedure.OutputParameters) != 0 {
		t.Fatalf("parameterless fixture unexpectedly has parameters: %#v/%#v", procedure.InputParameters, procedure.OutputParameters)
	}
	procedure.InputCount = sql.NullInt64{Int64: 0, Valid: true}
	procedure.OutputCount = sql.NullInt64{Int64: 0, Valid: true}
	ddl, err := procedure.GenerateDDLWithOptions(catalogschema.DDLOptions{Dialect: catalogschema.Dialect1})
	if err != nil {
		t.Fatalf("generate same-dialect procedure DDL: %v", err)
	}
	if !strings.HasSuffix(ddl, procedure.Source.String) {
		t.Fatalf("generated procedure changed source text:\nwant suffix %q\ngot %q", procedure.Source.String, ddl)
	}
	if _, err := replay.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("replay same-dialect procedure: %v\n%s", err, ddl)
	}
	recreated, err := catalogschema.New(replay).Procedure(ctx, "GO_DIALECT1_SOURCE")
	if err != nil || recreated == nil {
		t.Fatalf("load recreated procedure: %#v, %v", recreated, err)
	}
	if recreated.Source.String != procedure.Source.String {
		t.Fatalf("procedure source changed on replay: source=%q replay=%q", procedure.Source.String, recreated.Source.String)
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
