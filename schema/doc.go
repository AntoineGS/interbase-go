// Package schema provides bounded, read-only access to the InterBase system
// catalog through database/sql, plus executable DDL for metadata that can be
// represented without silently losing information.
//
// The package deliberately depends only on QueryContext. It does not import
// the native driver package and can be used with a *sql.DB, *sql.Conn, or
// *sql.Tx. The supplied queryer remains owned by the caller. Catalog methods
// never execute generated DDL or invoke external functions.
package schema
