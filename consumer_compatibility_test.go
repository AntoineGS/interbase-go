package interbase_test

import (
	"context"
	"database/sql"
	"testing"

	interbase "interbase-go"
)

// TestSQLsAndMDExplorerDriverAPI is the public compile contract for the one
// driver revision shared by SQLs and MDExplorer: SQLs relies on catalog text
// decoding via Config.CatalogTextCharset, and MDExplorer relies on the
// explicit NONE attachment charset plus statement-description inspection.
func TestSQLsAndMDExplorerDriverAPI(t *testing.T) {
	cfg := interbase.Config{Charset: "NONE", CatalogTextCharset: "WIN1252"}
	if cfg.Charset != "NONE" {
		t.Fatal(cfg.Charset)
	}
	var describe func(context.Context, *sql.Conn, string) (interbase.StatementDescriptor, error)
	describe = interbase.DescribeStatement
	if describe == nil {
		t.Fatal("missing statement inspection")
	}
}
