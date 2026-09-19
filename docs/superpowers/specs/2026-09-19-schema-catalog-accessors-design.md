# Schema Catalog Accessors Design

**Date:** 2026-09-19
**Status:** Approved design
**Baseline:** `fbcbfb7` (`main`)

## Purpose

Export two pieces of InterBase catalog decoding that the `schema` package
already performs internally, so that external consumers — the `sqls` language
server in particular — do not reimplement vendor catalog encodings:

- the human-readable trigger event decoded from `RDB$TRIGGERS.RDB$TRIGGER_TYPE`;
- the SQL type declaration of an external-function argument and of a function's
  return value.

A third candidate, the user-domain predicate `userDomainReference`
(`schema/ddl.go:407-412`), is evaluated and declined below.

The additions are pure Go value methods on existing types. They issue no query,
mutate nothing, and add no import beyond what `schema/ddl.go` already uses.
`schema` remains read-only and free of any cgo dependency (`schema/doc.go:5-8`).

## Evidence and Constraints

`triggerEvent` (`schema/ddl.go:871-913`) is the package's only trigger-type
decoder. Its actual behavior, read from the source rather than assumed:

- It rejects a negative code or any bit outside `(3<<13) | 0x7f`
  (`ddl.go:872-876`).
- When `code & (3<<13) == 1<<13` it decodes a database trigger and returns
  `"ON CONNECT"`, `"ON DISCONNECT"`, `"ON TRANSACTION START"`,
  `"ON TRANSACTION COMMIT"`, or `"ON TRANSACTION ROLLBACK"`, including the
  `ON ` prefix, and errors on any other low value (`ddl.go:877-883`).
- The remaining reserved mask values `2<<13` and `3<<13` are errors
  (`ddl.go:884-886`).
- Otherwise it decodes a DML trigger from `code+1`: bit 0 selects
  `BEFORE`/`AFTER`, and three 2-bit slots at shifts 1, 3 and 5 select
  `INSERT`/`UPDATE`/`DELETE` (`ddl.go:887-908`). A missing first operation or a
  non-contiguous operation list is an error (`ddl.go:898-906`).
- A multi-event trigger yields one joined string, for example
  `triggerEvent(17) == "BEFORE INSERT OR UPDATE"`
  (`schema/extended_schema_test.go:677-684`).

`Trigger.GenerateDDL` consumes that decoder and additionally refuses multi-event
codes, because InterBase trigger DDL cannot express them (`ddl.go:929-931`). The
event text itself is valid metadata in that case; only the generated statement
is not.

`Domain.sqlTypeParts` (`ddl.go:213-346`) is the package's only field-type
renderer. `Domain.SQLType` is a thin wrapper over it (`ddl.go:188-194`).
Relevant facts for function arguments:

- `CSTRING` (`fieldTypeCString`, code 40) is rejected with the explicit note
  that it "is only supported for external arguments" (`ddl.go:315-316`), so the
  domain renderer intentionally leaves this case to a caller that owns
  external-argument metadata.
- A character-typed field whose `CharacterSetID` is valid and non-zero but whose
  `CharacterSetName` is missing is rejected rather than rendered without a
  suffix (`ddl.go:331-333`).
- Character length is taken from `CharacterLength`, never from the byte
  `FieldLength`; a missing character length is an error (`ddl.go:287-293`), and
  the unit suite pins that refusal for a multibyte domain
  (`extended_schema_test.go:546-555`).

`FunctionArgument` (`catalog_extended.go:132-145`) carries `CharacterSetID` but
no `CharacterSetName`, because `functionArgumentsQuery`
(`catalog_extended.go:263-270`) selects `RDB$FUNCTION_ARGUMENTS` alone and does
not join `RDB$CHARACTER_SETS` the way `extendedDomainQuery` does
(`catalog_extended.go:188-193`). The name is therefore **not** obtainable from
what the package already reads. It also carries no segment length and no
dimensions column.

`Function.ReturnArgument` (`catalog_extended.go:124`) holds the *argument
position* of the return value, and `Arguments` is ordered by
`RDB$ARGUMENT_POSITION` (`catalog_extended.go:270`). The unit fixture's
`GO_EXTERNAL` declares `RDB$RETURN_ARGUMENT = 0` with a single argument row at
position 0 (`schema/schema_test.go:1594-1596`), and the live suite declares
`... GO_SCHEMA_EXT_FUNCTION INTEGER RETURNS INTEGER BY VALUE ...`
(`integration/schema_test.go:258`).

`Function.GenerateDDL` is intentionally unsupported because external calling
conventions are not fully captured (`ddl.go:1213-1218`). Nothing rendered here
is claimed to be an executable declaration.

## Scope

### Included

- `Trigger.Event`, `FunctionArgument.SQLType`, and `Function.ReturnType`.
- One internal extraction of the existing character-set guard so the new
  `CSTRING` branch reuses it instead of restating it.
- Error relabelling so an `*UnsupportedDDLError` raised through the reused
  domain renderer reports the function argument, not a synthetic domain.
- Server-independent table-driven unit tests and two live assertions.
- A `schema/README.md` paragraph.

### Excluded

- Any change to a catalog query, to `Catalog` methods, or to existing DDL
  output.
- Exporting `userDomainReference` (declined below).
- An exported input-argument filter, an exported `DataType` alias for
  `FunctionArgument`, or an exported structured trigger-event decomposition:
  each is a one-line derivation for the caller, and the package's public surface
  is deliberately bounded.
- Rendering UDF calling conventions (`BY VALUE`, `BY DESCRIPTOR`, `FREE_IT`),
  which remain covered by the existing refusal at `ddl.go:1216-1217`.

## Design

### 1. `Trigger.Event`

```go
// Event returns the decoded trigger event, for example "BEFORE INSERT",
// "BEFORE INSERT OR UPDATE", or "ON CONNECT" for a database trigger.
func (t Trigger) Event() (string, error)
```

A method on `Trigger` rather than a package function over a raw code: the NULL
check belongs with the catalog value, and a method cannot be handed an integer
from an unrelated catalog column by mistake.

Behavior:

- `TriggerType` invalid (NULL) returns
  `unsupportedDDL("trigger", t.Name, "trigger type is NULL")`, the same error
  `GenerateDDL` already produces for that input (`ddl.go:922-924`).
- Otherwise it calls `triggerEvent(t.TriggerType.Int64)` — the existing
  unexported decoder, unchanged and uncopied — and on failure wraps the
  decoder's message with `unsupportedDDL("trigger", t.Name, err.Error())`,
  exactly as `GenerateDDL` does (`ddl.go:925-928`). Every failure therefore
  matches `errors.Is(err, ErrUnsupportedDDL)`, consistent with the rest of the
  package.
- A database trigger returns the decoder's `ON ...` form. Such triggers have a
  NULL `RelationName`, which callers already observe on the struct.
- A multi-event trigger returns the decoder's single joined string
  (`"BEFORE INSERT OR UPDATE"`). Returning a `[]string` would require splitting
  or re-decoding the operation slots in a second place, which is precisely the
  duplication this design forbids; a caller that needs the parts can split on
  `" OR "`.
- `Event` deliberately succeeds for multi-event codes even though
  `GenerateDDL` rejects them (`ddl.go:929-931`): the event is real metadata, only
  the CREATE TRIGGER statement is not expressible.

Because `GenerateDDL` and `Event` call the same decoder, the two cannot drift.

### 2. `FunctionArgument.SQLType` and `Function.ReturnType`

```go
// SQLType renders the SQL declaration of an external function argument, for
// example "INTEGER" or "CSTRING(80)".
func (a FunctionArgument) SQLType() (string, error)

// ReturnType renders the SQL declaration of the function's return value.
func (f Function) ReturnType() (string, error)
```

`SQLType` mirrors `Domain.SQLType` so the two renderers read alike at call
sites. `ReturnType` is a method on `Function` because resolving
`ReturnArgument` to an argument row is the encoding detail callers should not
repeat.

**Reuse.** `SQLType` builds an unexported `Domain` value from the argument's
`RDB$FIELDS`-shaped columns and calls `sqlTypeParts` (`ddl.go:213-346`),
rendering with `parts.render(true)` as `Domain.SQLType` does (`ddl.go:193`).
The mapping is `Name`, `FieldType`, `FieldSubType`, `FieldScale`,
`FieldLength`, `FieldPrecision`, `CharacterLength`, and `CharacterSetID`; the
remaining `Domain` fields stay zero. No second type switch exists.

Two consequences are stated rather than papered over: `RDB$FUNCTION_ARGUMENTS`
carries no segment length in this projection, so a BLOB argument renders as
`BLOB SUB_TYPE ...` without `SEGMENT SIZE`; and it carries no dimensions, so the
array guard at `ddl.go:214-216` cannot fire for an argument.

**CSTRING.** `sqlTypeParts` rejects code 40 by design (`ddl.go:315-316`), so
`SQLType` handles it before delegating, and only it:

- the length is `CharacterLength` when valid and positive, otherwise
  `FieldLength` when valid and positive, otherwise `ErrUnsupportedDDL`. This is
  not a byte-for-character substitution of the kind the domain renderer refuses:
  the InterBase `CSTRING(n)` grammar declares a byte length, and the character-set
  guard below has already established that any rendered argument uses the default
  single-byte character set;
- the character-set guard is applied through the same helper as the delegated
  path (below).

The contract is that the rendered length equals the declared length. The live
test named in *Testing Strategy* is the acceptance gate for that contract on the
tested stack; if it fails, the fix is to change which catalog column the rule
prefers and to update this section and the unit table, never to introduce a
silent `±1` adjustment.

**Character set.** The name is unavailable, as established above. The renderer
therefore never emits a character-set suffix for an argument, and it refuses to
render a character-typed argument whose `CharacterSetID` is valid and non-zero,
returning `ErrUnsupportedDDL` with feature text `character set name is
unavailable`. That is the identical rule the domain renderer already applies
when a name is missing (`ddl.go:331-333`), so a `CHAR`, `VARCHAR`, or `BLOB`
argument reaches it through the delegated path with no new code at all.

To apply the same rule in the new `CSTRING` branch without a second copy, the
guard at `ddl.go:325-333` is **moved** into

```go
func characterSetClause(object, name string, setName sql.NullString, setID sql.NullInt64) (string, error)
```

which returns the quoted character-set name, `""`, or the unsupported error.
`sqlTypeParts` calls it with the domain's own values; the `CSTRING` branch calls
it with an invalid `setName` and the argument's `CharacterSetID`. This is a
move, not a duplicate: one body, two call sites.

Joining `RDB$CHARACTER_SETS` into `functionArgumentsQuery` and adding a
`CharacterSetName` field would make the suffix renderable, and it remains the
obvious strictly-additive follow-up. It is out of scope here because it changes
an existing query, an existing struct, and the unit fixture for a case a plain
`CSTRING` or `INTEGER` declaration does not reach, and the refusal above is
correct until evidence shows that path is common.

**Errors.** When the delegated `sqlTypeParts` call fails, `SQLType` re-targets
the error rather than nesting it: if `errors.As` yields an
`*UnsupportedDDLError`, it returns
`unsupportedDDL("function argument", a.Name, unsupported.Feature)`; any other
error is returned unchanged. The caller's `errors.As` then reports
`Object: "function argument"` with the real reason, and the synthetic domain
never appears in a message. Every unsupported case here wraps
`ErrUnsupportedDDL`, as elsewhere in the package.

**`ReturnType`.** It returns `unsupportedDDL("external function", f.Name, ...)`
when `ReturnArgument` is NULL, and likewise when no loaded argument has
`Position` equal to `ReturnArgument`. Otherwise it delegates to that argument's
`SQLType`. The lookup matches on `Position`, not on slice index: the two
coincide only when the rows are dense from zero, and when `RDB$RETURN_ARGUMENT`
is `N > 0` the return value *is* argument `N`, which is also an input. The first
row whose `Position` matches wins; catalog positions are unique per function.

### 3. Exporting `userDomainReference` — declined

`userDomainReference` is not exported. Three reasons, and the third is decisive:

1. It is three cheap conditions, not a vendor encoding: a nil/empty name, an
   `RDB$` prefix, and a system flag that is treated as *user* when NULL
   (`ddl.go:407-412`). There is nothing here a caller can get subtly wrong the
   way it can with a trigger-type bitfield.
2. `Domain.Name` and `Domain.SystemFlag` are already exported
   (`schema.go:102,111`), so the test is directly expressible by a caller.
3. It does not, by itself, answer the question `sqls` is asking. Both call sites
   pair it with a separate non-empty `FieldSource` check
   (`ddl.go:440`, `ddl.go:780`), because a resolved domain value is not the same
   thing as a column or parameter that *references* a named domain. An exported
   `userDomainReference` would be a half-predicate inviting the exact mistake it
   appears to prevent.

`sqls` should test `column.FieldSource` for a non-empty value and
`column.Domain` for a non-`RDB$` name with a zero or NULL system flag. If a
future consumer needs the full column-level test, the right export is a
predicate over `Column`/`ProcedureParameter`, not over `*Domain`.

## Testing Strategy

The new logic is decided entirely by struct values, so all of it is covered by
the server-independent unit suite, which uses the deterministic local
`database/sql/driver` fixture (`schema/README.md:113-115`,
`schema/schema_test.go:763-776`). No new test requires a live server.

### Unit tests (`schema/extended_schema_test.go`)

Table-driven, in the style of `TestDDLRejectsAmbiguousAndLossyTypeMetadata`
(`extended_schema_test.go:533-563`).

`Trigger.Event`, one table over trigger-type codes:

- `1` → `BEFORE INSERT`; `2` → `AFTER INSERT`; `17` → `BEFORE INSERT OR UPDATE`;
- `1<<13` → `ON CONNECT`; `(1<<13)+4` → `ON TRANSACTION ROLLBACK`;
- invalid `TriggerType` (NULL), `0` (no operation), `1<<7` (reserved bits),
  `-1` (negative), `2<<13` and `3<<13` (reserved mask values) → error matching
  `errors.Is(err, ErrUnsupportedDDL)`.

Plus one drift guard: for each single-event DML code in the table, the string
returned by `Event` is contained in the output of `GenerateDDL` for an otherwise
complete `Trigger`.

`FunctionArgument.SQLType`, one table:

- `INTEGER`; `NUMERIC(9, 2)` via subtype and scale; `VARCHAR(20)` with a valid
  `CharacterLength`; `CSTRING(80)` from `FieldLength` with `CharacterLength`
  NULL; `CSTRING(80)` from a valid `CharacterLength`; `BLOB SUB_TYPE TEXT`;
- unsupported: NULL `FieldType`; unknown field type; `9` (QUAD); `VARCHAR` with
  NULL `CharacterLength`; `VARCHAR` with `CharacterSetID = 4`; `CSTRING` with
  `CharacterSetID = 4`; `CSTRING` with NULL and with zero `FieldLength`.

One case additionally asserts through `errors.As` that the returned
`*UnsupportedDDLError` has `Object == "function argument"` and the argument's
name, proving the relabelling.

`Function.ReturnType`, one table: return argument at position 0; return argument
at a positive position shared with an input; NULL `ReturnArgument`; and a
`ReturnArgument` with no matching row — the last two unsupported.

Fixture-backed coverage, so the accessors are exercised on scanned catalog
values and not only on hand-built structs:

- `Functions(ctx, "GO_EXTERNAL")` already yields one position-0 `INTEGER`
  argument with `RDB$RETURN_ARGUMENT = 0` (`schema_test.go:1594-1596`); assert
  `ReturnType() == "INTEGER"`.
- Add a second fixture external function with a `CSTRING` argument row rather
  than adding a row to `GO_EXTERNAL`, so the existing argument-count assertion
  at `extended_schema_test.go:91-93` keeps holding unchanged.
- `Triggers(ctx, "GO_CHILD_BI")` already returns a scanned trigger
  (`extended_schema_test.go:60-66`); assert its `Event()`.

### Live coverage

Two assertions are warranted, in `integration/schema_test.go` — not
`integration/metadata_test.go`, which covers driver result-set metadata rather
than the `schema` package — inside `TestSchemaExtendedCatalogFamiliesAndDDL`
(`integration/schema_test.go:225`), whose fixture already creates both object
kinds:

- `GO_SCHEMA_EXT_TRIGGER` is declared `ACTIVE BEFORE INSERT`
  (`integration/schema_test.go:252-253`); assert `Event() == "BEFORE INSERT"`.
- `GO_SCHEMA_EXT_FUNCTION` is declared `INTEGER RETURNS INTEGER BY VALUE`
  (`integration/schema_test.go:258`); assert `ReturnType() == "INTEGER"` and that
  the position-0 argument renders `INTEGER`.

One declaration is added to the same `createSchemaObjects` list to pin the
`CSTRING` rule against a real catalog:

```sql
DECLARE EXTERNAL FUNCTION GO_SCHEMA_EXT_CSTRING CSTRING(80)
RETURNS CSTRING(80) FREE_IT
ENTRY_POINT 'go_schema_ext_cstring' MODULE_NAME 'go_schema_ext_library'
```

Assert that both the argument and `ReturnType()` render exactly `CSTRING(80)`.
Declaring an external function records metadata only; no module is loaded and no
UDF is invoked, consistent with `schema/README.md:56-57` and the existing
declaration at line 258.

## Documentation and Compatibility

All three additions are new methods on existing exported types; no signature,
output, or error of any existing API changes. `characterSetClause` is internal.

`schema/README.md` gains a short paragraph under *Extended catalog objects*
stating that `Trigger.Event` decodes the catalog trigger type including
database-level and multi-event forms, that `FunctionArgument.SQLType` and
`Function.ReturnType` render external-argument declarations, that these render
no character-set suffix because `RDB$FUNCTION_ARGUMENTS` supplies no
character-set name and refuse to render an argument declared under a
non-default character set, and that unsupported metadata continues to wrap
`ErrUnsupportedDDL`.
