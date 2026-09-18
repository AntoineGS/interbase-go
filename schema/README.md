# `schema`

`schema` is a small, pure-Go read-only InterBase catalog reader. It has no
dependency on the native cgo package or on a particular driver implementation.
It generates DDL only where the catalog supplies enough information to preserve
the declaration without guessing; it is not a general schema migration engine.

## Usage

```go
catalog := schema.New(db) // db may be *sql.DB, *sql.Conn, or *sql.Tx

tables, err := catalog.Tables(ctx, "ORDERS") // empty name lists all tables
if err != nil {
	return err
}
for _, table := range tables {
	fmt.Println(table.Name, len(table.Columns))
}

procedures, err := catalog.Procedures(ctx, "")
```

Names are matched exactly. Catalog identifiers are right-trimmed for fixed
catalog padding while preserving their case. SQL text fields such as
`DefaultSource`, `ComputedSource`, `ViewSource`, and `Procedure.Source` are
returned as `sql.NullString` values without trimming.
Nullable numeric, string, and boolean catalog metadata uses the corresponding
`database/sql` nullable type. `Column.Domain` and
`ProcedureParameter.Domain` contain the resolved `RDB$FIELDS` type attributes.
Procedure parameter declaration nullability is not present in the
reference-compatible catalog projection; `ProcedureParameter.Nullable` is
therefore unknown except when a non-nullable domain proves the parameter
cannot accept NULL.

`Relations` returns both tables and views; `Tables` and `Views` restrict the
kind. The singular `Table`, `View`, and `Procedure` helpers return `nil, nil`
for an unknown user object. `Table` additionally loads constraints, indexes,
and triggers so the returned relation is suitable for `GenerateDDL`; list
methods intentionally retain their bounded column-only behavior. The package
does not expose system objects.

## Extended catalog objects

The package also provides exact-name and list queries for:

- domains and their character-set/collation metadata;
- generators/sequences and ordered index segments;
- relation constraints, including foreign-key partners and check sources;
- triggers, including their preserved PSQL source;
- roles, dependencies, external-function declarations, database files, and
  shadows; and
- user privileges.

These methods use the official `RDB$...` catalogs, bind all name filters, and
fully consume each result before loading child metadata. External functions are
only inspected; no UDF is ever loaded or called by this package.

## DDL generation

Objects implementing `DDLer` expose `GenerateDDL()` for domains, tables/views,
procedures, triggers, indexes, generators/sequences, roles, and privileges.
Identifiers are always quoted and embedded single quotes are escaped. Catalog
source/default/check text is preserved rather than reformatted. InterBase
uses `CREATE GENERATOR` for the catalog object represented by `Sequence`.
`Index` also implements `Statementer`; use `Statements()` when applying an
index definition independently, because an inactive index requires `CREATE`
followed by `ALTER INDEX ... INACTIVE`. It returns `([]string, error)` so
invalid or unsupported catalog metadata cannot be mistaken for an empty
definition. `GenerateDDL()` retains the single-string form by joining those
statements.

Computed columns and procedure parameters whose declaration nullability is
unknown return an error wrapping `ErrUnsupportedDDL`; the catalog does not
contain enough information to reproduce those declarations faithfully. Named
NOT NULL constraints are rendered inline on their matching column, while
InterBase-generated `INTEG_*` names are emitted as bare `NOT NULL` because the
server rejects re-creating those system-generated names.

`Function`, `DatabaseFile`, and `Shadow` intentionally return an error wrapping
`ErrUnsupportedDDL`: their complete creation semantics include platform,
filesystem, or external calling-convention policy that this read-only package
does not infer. Callers should check with `errors.Is(err, schema.ErrUnsupportedDDL)`
and choose an explicit administrative policy.

## Consistency and lifetime

All catalog queries are read-only and all result sets are fully consumed and
closed before a child query is issued. For a consistent metadata read, the
caller can begin an explicit transaction and pass its `*sql.Tx` to `New`:

```go
tx, err := db.BeginTx(ctx, opts)
if err != nil {
	return err
}
defer tx.Rollback()

catalog := schema.New(tx)
// Read relations, columns, and procedures through this catalog.
```

The transaction and its isolation options remain caller-owned. This package
does not claim that the current default read-committed behavior provides a
snapshot; choose and verify the transaction policy appropriate for the server
and workload.

## Scope and consistency

This package remains read-only: it does not create, alter, or drop database
objects, and `GenerateDDL` only returns text. It does not promise full Python
`interbase.schema` parity or database/service administration. The live
integration suite validates the supported catalog projections against a
disposable InterBase server; the unit suite remains independent of a server
and uses a deterministic local `database/sql/driver` fixture.
