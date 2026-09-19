# Schema Catalog Accessors Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Export three read-only accessors — `Trigger.Event`, `FunctionArgument.SQLType`, and `Function.ReturnType` — so external consumers read InterBase catalog encodings the `schema` package already decodes instead of reimplementing them.

**Architecture:** All three are pure Go value methods added to `schema/ddl.go`, each delegating to the package's existing single decoder: `Event` calls the unexported `triggerEvent`, and `SQLType` builds a synthetic unexported `Domain` and calls the unexported `Domain.sqlTypeParts` for every non-CSTRING type. One supporting refactor hoists the character-set guard out of `sqlTypeParts` into a `characterSetClause` helper so the new CSTRING branch shares that body rather than restating it. `ReturnType` resolves `RDB$RETURN_ARGUMENT` against argument `Position` and delegates to `SQLType`.

**Tech Stack:** Go, `database/sql`, no cgo, no new imports, deterministic local `database/sql/driver` test fixture, `integration` build tag for live coverage.

**Spec:** `docs/superpowers/specs/2026-09-19-schema-catalog-accessors-design.md`

## Global Constraints

- The spec's stated baseline is commit `fbcbfb7` on `main`. Every commit since is documentation only — the spec, its revision, and this plan — and no production file differs from `fbcbfb7`. Branch from `main` as it stands.
- The additions "are pure Go value methods on existing types. They issue no query, mutate nothing, and add no import beyond what `schema/ddl.go` already uses."
- "`schema` remains read-only and free of any cgo dependency." The `schema` unit suite must keep building and passing with `CGO_ENABLED=0`.
- Excluded: "Any change to a catalog query, to `Catalog` methods, or to existing DDL output." No existing signature, output, or error may change.
- Reuse is mandatory: "No second type switch exists." `triggerEvent` and `Domain.sqlTypeParts` remain the single source of truth; copying either body is a plan failure. The character-set guard is "a move, not a duplicate: one body, two call sites."
- CSTRING length: "The rendered length is `FieldLength` verbatim, never adjusted." `CharacterLength` is not consulted, and "A defensive `CharacterLength` branch is deliberately *not* specified."
- If the live `CSTRING(80)` assertion fails: "the fix is to revisit which catalog column the rule reads and to update this section and the unit table, never to introduce a silent `±1` adjustment."
- Character set: the renderer "never emits a character-set suffix for an argument, and it refuses to render a character-typed argument whose `CharacterSetID` is valid and non-zero, returning `ErrUnsupportedDDL` with feature text `character set name is unavailable`."
- CHAR and VARCHAR arguments: "On these catalogs every CHAR and VARCHAR argument therefore returns `ErrUnsupportedDDL`, whatever its character set." This is expected behavior; tests assert it rather than work around it.
- `ReturnType` "lookup matches on `Position`, not on slice index." Writing `Arguments[ReturnArgument]` is wrong.
- `Trigger.Event` returns the decoder's single joined string for multi-event codes and "deliberately succeeds for multi-event codes even though `GenerateDDL` rejects them."
- "Every unsupported case here wraps `ErrUnsupportedDDL`, as elsewhere in the package" — built with `unsupportedDDL(object, name, feature)` (`schema/ddl.go:51-53`).
- `userDomainReference` is not exported.
- Also excluded: "An exported input-argument filter, an exported `DataType` alias for `FunctionArgument`, or an exported structured trigger-event decomposition."
- Use TDD for every behavior change: observe the intended test fail before writing production code.
- Commit only the files listed in each task's commit step. Another agent may be working in this repository; never use `git add -A` or `git add .`.

---

## File Structure

- `schema/ddl.go`: the only production file that changes. Gains `characterSetClause` (moved guard body), `Trigger.Event` beside `triggerEvent`, and `FunctionArgument.SQLType` / `Function.ReturnType` beside `Function.GenerateDDL`.
- `schema/extended_schema_test.go`: all new unit tests. It already holds the extended-catalog and DDL-refusal tables the new tables are modeled on.
- `schema/schema_test.go`: fixture data only — one new external function in `fixtureFunctions()` so the accessors are exercised on scanned values.
- `integration/schema_test.go`: one new `DECLARE EXTERNAL FUNCTION` in the existing `TestSchemaExtendedCatalogFamiliesAndDDL` fixture, plus live assertions.
- `schema/README.md`: one paragraph under *Extended catalog objects*.

No new file is created. The package's existing layout puts all rendering logic in `ddl.go`; splitting it would contradict the surrounding code.

---

### Task 1: Move the character-set guard into `characterSetClause`

This is a behavior-preserving move. It ships first so Task 3's CSTRING branch has one body to call.

**Files:**
- Modify: `schema/ddl.go:323-334` (the guard inside `Domain.sqlTypeParts`)
- Test: `schema/extended_schema_test.go`

**Interfaces:**
- Consumes: `unsupportedDDL(object, name, feature string) error` (`ddl.go:51`), `quoteRequiredIdentifier(name, label string) (string, error)` (`ddl.go:65`).
- Produces: `func characterSetClause(object, name string, setName sql.NullString, setID sql.NullInt64) (string, error)` — returns the quoted character-set name, `""` when there is no suffix to emit, or an `*UnsupportedDDLError` labelled with `object`/`name`.

- [ ] **Step 1: Write the failing test**

Append to `schema/extended_schema_test.go`:

```go
func TestCharacterSetClauseSharesOneGuardBody(t *testing.T) {
	quoted, err := characterSetClause("domain", "D", sql.NullString{String: `WEIRD"SET`, Valid: true}, sql.NullInt64{Int64: 4, Valid: true})
	if err != nil || quoted != `"WEIRD""SET"` {
		t.Fatalf("named character set = (%q, %v), want quoted name", quoted, err)
	}

	if quoted, err := characterSetClause("domain", "D", sql.NullString{String: "   ", Valid: true}, sql.NullInt64{}); err != nil || quoted != "" {
		t.Fatalf("blank name with no id = (%q, %v), want empty clause", quoted, err)
	}

	if quoted, err := characterSetClause("domain", "D", sql.NullString{}, sql.NullInt64{Int64: 0, Valid: true}); err != nil || quoted != "" {
		t.Fatalf("zero character set id = (%q, %v), want empty clause", quoted, err)
	}

	_, err = characterSetClause("function argument", "F_0", sql.NullString{}, sql.NullInt64{Int64: 4, Valid: true})
	var unsupported *UnsupportedDDLError
	if !errors.As(err, &unsupported) {
		t.Fatalf("missing name with non-zero id error = %v, want *UnsupportedDDLError", err)
	}
	if unsupported.Object != "function argument" || unsupported.Name != "F_0" || unsupported.Feature != "character set name is unavailable" {
		t.Fatalf("unsupported error = %#v, want caller-supplied object, name, and unavailable-name feature", unsupported)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=0 go test ./schema/... -count=1 -run '^TestCharacterSetClauseSharesOneGuardBody$' -v`

Expected: FAIL — build error `undefined: characterSetClause`.

- [ ] **Step 3: Add the helper**

Insert into `schema/ddl.go` immediately after `Domain.sqlTypeParts` ends (after the current line 346):

```go
func characterSetClause(object, name string, setName sql.NullString, setID sql.NullInt64) (string, error) {
	if setName.Valid && strings.TrimSpace(setName.String) != "" {
		charset, err := quoteRequiredIdentifier(strings.TrimRight(setName.String, " "), "character set")
		if err != nil {
			return "", err
		}
		return charset, nil
	}
	if setID.Valid && setID.Int64 != 0 {
		return "", unsupportedDDL(object, name, "character set name is unavailable")
	}
	return "", nil
}
```

- [ ] **Step 4: Move the guard's call site**

In `Domain.sqlTypeParts`, replace the block currently at `schema/ddl.go:323-334`:

```go
	parts := sqlTypeParts{base: result}
	if fieldType == fieldTypeChar || fieldType == fieldTypeVarchar || fieldType == fieldTypeBlob {
		if d.CharacterSetName.Valid && strings.TrimSpace(d.CharacterSetName.String) != "" {
			charset, err := quoteRequiredIdentifier(strings.TrimRight(d.CharacterSetName.String, " "), "character set")
			if err != nil {
				return sqlTypeParts{}, err
			}
			parts.charset = charset
		} else if d.CharacterSetID.Valid && d.CharacterSetID.Int64 != 0 {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "character set name is unavailable")
		}
	}
```

with:

```go
	parts := sqlTypeParts{base: result}
	if fieldType == fieldTypeChar || fieldType == fieldTypeVarchar || fieldType == fieldTypeBlob {
		charset, err := characterSetClause("domain", d.Name, d.CharacterSetName, d.CharacterSetID)
		if err != nil {
			return sqlTypeParts{}, err
		}
		parts.charset = charset
	}
```

Leave the collation block that follows (`ddl.go:335-344`) untouched. No other line of `sqlTypeParts` changes.

- [ ] **Step 5: Run the new test and the DDL regression tests**

Run:

```sh
CGO_ENABLED=0 go test ./schema/... -count=1 -run '^TestCharacterSetClauseSharesOneGuardBody$' -v
CGO_ENABLED=0 go test ./schema/... -count=1 -run '^TestDDLGenerationQuotesIdentifiersAndPreservesSupportedClauses$|^TestDDLRejectsAmbiguousAndLossyTypeMetadata$' -v
```

Expected: PASS for all. The second command is the regression gate for this move: `TestDDLGenerationQuotesIdentifiersAndPreservesSupportedClauses` pins the exact string `CREATE DOMAIN "Weird""Domain" AS VARCHAR(20) CHARACTER SET "UTF8" ...` (`extended_schema_test.go:267`), and `TestDDLRejectsAmbiguousAndLossyTypeMetadata` pins the multibyte refusal (`extended_schema_test.go:546-555`). If either changed, the move was not behavior-preserving — revert and redo it, do not adjust the expectation.

- [ ] **Step 6: Run the whole schema suite and vet**

Run:

```sh
CGO_ENABLED=0 go test ./schema/... -count=1
CGO_ENABLED=0 go vet ./schema/...
```

Expected: `ok  interbase-go/schema` and no vet output.

- [ ] **Step 7: Commit**

```bash
git add schema/ddl.go schema/extended_schema_test.go
git commit -m "Share one character-set guard body between domain and argument rendering"
```

---

### Task 2: `Trigger.Event`

**Files:**
- Modify: `schema/ddl.go` (insert between `triggerEvent`, ending at line 913, and `Trigger.GenerateDDL`, starting at line 915)
- Test: `schema/extended_schema_test.go`

**Interfaces:**
- Consumes: `triggerEvent(triggerType int64) (string, error)` (`ddl.go:871`), `unsupportedDDL` (`ddl.go:51`), `Trigger` (`catalog_extended.go:88-98`) with `Name string` and `TriggerType sql.NullInt64`.
- Produces: `func (t Trigger) Event() (string, error)` — the decoded event text, for example `"BEFORE INSERT"`, `"BEFORE INSERT OR UPDATE"`, or `"ON CONNECT"`. Every error wraps `ErrUnsupportedDDL`.

- [ ] **Step 1: Write the failing test**

Append to `schema/extended_schema_test.go`:

```go
func TestTriggerEventExportsDecodedCatalogEvent(t *testing.T) {
	rendered := []struct {
		name        string
		triggerType int64
		want        string
	}{
		{name: "before insert", triggerType: 1, want: "BEFORE INSERT"},
		{name: "after insert", triggerType: 2, want: "AFTER INSERT"},
		{name: "multi event", triggerType: 17, want: "BEFORE INSERT OR UPDATE"},
		{name: "database connect", triggerType: 1 << 13, want: "ON CONNECT"},
		{name: "transaction rollback", triggerType: (1 << 13) + 4, want: "ON TRANSACTION ROLLBACK"},
	}
	for _, test := range rendered {
		t.Run(test.name, func(t *testing.T) {
			trigger := Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: test.triggerType, Valid: true}}
			got, err := trigger.Event()
			if err != nil || got != test.want {
				t.Fatalf("Event = (%q, %v), want (%q, nil)", got, err, test.want)
			}
		})
	}

	unsupported := []struct {
		name    string
		trigger Trigger
	}{
		{name: "null trigger type", trigger: Trigger{Name: "EVENT_TRIGGER"}},
		{name: "no operation", trigger: Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: 0, Valid: true}}},
		{name: "reserved bits", trigger: Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: 1 << 7, Valid: true}}},
		{name: "negative code", trigger: Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: -1, Valid: true}}},
		{name: "reserved mask two", trigger: Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: 2 << 13, Valid: true}}},
		{name: "reserved mask three", trigger: Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: 3 << 13, Valid: true}}},
	}
	for _, test := range unsupported {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.trigger.Event(); !errors.Is(err, ErrUnsupportedDDL) {
				t.Fatalf("Event error = %v, want ErrUnsupportedDDL", err)
			}
		})
	}

	// Drift guard: GenerateDDL and Event must keep decoding through the same
	// body, so every single-event string Event returns has to appear verbatim
	// in the generated statement.
	for _, test := range rendered[:2] {
		t.Run("ddl agrees on "+test.name, func(t *testing.T) {
			trigger := Trigger{
				Name:         "EVENT_TRIGGER",
				RelationName: sql.NullString{String: "GO_CHILD", Valid: true},
				TriggerType:  sql.NullInt64{Int64: test.triggerType, Valid: true},
				Sequence:     sql.NullInt64{Int64: 0, Valid: true},
				Source:       sql.NullString{String: "AS BEGIN END", Valid: true},
			}
			event, err := trigger.Event()
			if err != nil {
				t.Fatalf("Event returned error: %v", err)
			}
			ddl, err := trigger.GenerateDDL()
			if err != nil {
				t.Fatalf("GenerateDDL returned error: %v", err)
			}
			if !strings.Contains(ddl, event) {
				t.Fatalf("GenerateDDL = %q, want it to contain Event result %q", ddl, event)
			}
		})
	}

	db := openFixtureDB(t)
	defer db.Close()
	triggers, err := New(db).Triggers(context.Background(), "GO_CHILD_BI")
	if err != nil {
		t.Fatalf("Triggers returned error: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("triggers = %#v, want exactly one scanned trigger", triggers)
	}
	if got, err := triggers[0].Event(); err != nil || got != "BEFORE INSERT" {
		t.Fatalf("scanned trigger Event = (%q, %v), want (\"BEFORE INSERT\", nil)", got, err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=0 go test ./schema/... -count=1 -run '^TestTriggerEventExportsDecodedCatalogEvent$' -v`

Expected: FAIL — build error `trigger.Event undefined (type Trigger has no field or method Event)`.

- [ ] **Step 3: Implement `Event`**

Insert into `schema/ddl.go` between `triggerEvent` (ends line 913) and the `GenerateDDL` comment for `Trigger` (starts line 915):

```go
// Event returns the decoded trigger event, for example "BEFORE INSERT",
// "BEFORE INSERT OR UPDATE", or "ON CONNECT" for a database trigger. Unlike
// GenerateDDL it accepts a multi-event code: the event is real catalog
// metadata even where InterBase trigger DDL cannot express it. Callers that
// need the individual operations can split the result on " OR ".
func (t Trigger) Event() (string, error) {
	if !t.TriggerType.Valid {
		return "", unsupportedDDL("trigger", t.Name, "trigger type is NULL")
	}
	event, err := triggerEvent(t.TriggerType.Int64)
	if err != nil {
		return "", unsupportedDDL("trigger", t.Name, err.Error())
	}
	return event, nil
}
```

Do not touch `triggerEvent` or `Trigger.GenerateDDL`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=0 go test ./schema/... -count=1 -run '^TestTriggerEventExportsDecodedCatalogEvent$' -v`

Expected: PASS, including every subtest.

- [ ] **Step 5: Run the whole schema suite and vet**

Run:

```sh
CGO_ENABLED=0 go test ./schema/... -count=1
CGO_ENABLED=0 go vet ./schema/...
```

Expected: `ok  interbase-go/schema` and no vet output.

- [ ] **Step 6: Commit**

```bash
git add schema/ddl.go schema/extended_schema_test.go
git commit -m "Export decoded trigger event as Trigger.Event"
```

---

### Task 3: `FunctionArgument.SQLType`

**Files:**
- Modify: `schema/ddl.go` (insert before `Function.GenerateDDL`, currently at line 1213)
- Test: `schema/extended_schema_test.go`

**Interfaces:**
- Consumes: `Domain.sqlTypeParts() (sqlTypeParts, error)` (`ddl.go:213`), `sqlTypeParts.render(includeCollation bool) string` (`ddl.go:202`), `characterSetClause` from Task 1, `fieldTypeCString = int64(40)` (`ddl.go:168`), `unsupportedDDL` (`ddl.go:51`), `UnsupportedDDLError` (`ddl.go:19-23`), `FunctionArgument` (`catalog_extended.go:133-145`).
- Produces: `func (a FunctionArgument) SQLType() (string, error)` — for example `"INTEGER"`, `"CSTRING(80)"`, `"BLOB SUB_TYPE TEXT"`. Every failure is an `*UnsupportedDDLError` with `Object == "function argument"`, or a non-DDL error returned unchanged.

Note on `Name`: the package synthesizes each argument's `Name` as `<FUNCTION>_<position>` (`catalog_extended.go:935-938`), so error messages read like `function argument "GO_EXTERNAL_0"`.

- [ ] **Step 1: Write the failing test**

Append to `schema/extended_schema_test.go`:

```go
func TestFunctionArgumentSQLTypeRendersExternalDeclarations(t *testing.T) {
	rendered := []struct {
		name     string
		argument FunctionArgument
		want     string
	}{
		{
			name: "integer",
			argument: FunctionArgument{
				Name:           "F_INT_0",
				FieldType:      sql.NullInt64{Int64: fieldTypeInteger, Valid: true},
				FieldSubType:   sql.NullInt64{Int64: 0, Valid: true},
				FieldScale:     sql.NullInt64{Int64: 0, Valid: true},
				FieldLength:    sql.NullInt64{Int64: 4, Valid: true},
				FieldPrecision: sql.NullInt64{Int64: 10, Valid: true},
			},
			want: "INTEGER",
		},
		{
			name: "numeric from subtype and scale",
			argument: FunctionArgument{
				Name:           "F_NUM_0",
				FieldType:      sql.NullInt64{Int64: fieldTypeInteger, Valid: true},
				FieldSubType:   sql.NullInt64{Int64: 1, Valid: true},
				FieldScale:     sql.NullInt64{Int64: -2, Valid: true},
				FieldPrecision: sql.NullInt64{Int64: 9, Valid: true},
			},
			want: "NUMERIC(9, 2)",
		},
		{
			// The shape every measured CSTRING row has: RDB$CHARACTER_LENGTH
			// NULL, RDB$FIELD_LENGTH carrying the declared length verbatim.
			name: "cstring from field length",
			argument: FunctionArgument{
				Name:        "F_LEFT_1",
				FieldType:   sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength: sql.NullInt64{Int64: 80, Valid: true},
			},
			want: "CSTRING(80)",
		},
		{
			name: "cstring at the observed maximum",
			argument: FunctionArgument{
				Name:        "F_BIGSTRINGREPLACE_1",
				FieldType:   sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength: sql.NullInt64{Int64: 32000, Valid: true},
			},
			want: "CSTRING(32000)",
		},
		{
			name: "cstring at the observed minimum",
			argument: FunctionArgument{
				Name:        "F_CRLF_0",
				FieldType:   sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength: sql.NullInt64{Int64: 3, Valid: true},
			},
			want: "CSTRING(3)",
		},
		{
			// BLOB arguments carry RDB$FIELD_LENGTH 0; the BLOB branch reads
			// no length, so that is harmless.
			name: "blob without subtype",
			argument: FunctionArgument{
				Name:        "F_BLOB_0",
				FieldType:   sql.NullInt64{Int64: fieldTypeBlob, Valid: true},
				FieldLength: sql.NullInt64{Int64: 0, Valid: true},
			},
			want: "BLOB",
		},
		{
			name: "text blob",
			argument: FunctionArgument{
				Name:         "F_BLOB_1",
				FieldType:    sql.NullInt64{Int64: fieldTypeBlob, Valid: true},
				FieldSubType: sql.NullInt64{Int64: 1, Valid: true},
				FieldLength:  sql.NullInt64{Int64: 0, Valid: true},
			},
			want: "BLOB SUB_TYPE TEXT",
		},
	}
	for _, test := range rendered {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.argument.SQLType()
			if err != nil || got != test.want {
				t.Fatalf("SQLType = (%q, %v), want (%q, nil)", got, err, test.want)
			}
		})
	}

	unsupported := []struct {
		name     string
		argument FunctionArgument
	}{
		{
			name:     "null field type",
			argument: FunctionArgument{Name: "F_NULL_0"},
		},
		{
			name:     "unknown field type",
			argument: FunctionArgument{Name: "F_UNKNOWN_0", FieldType: sql.NullInt64{Int64: 99, Valid: true}},
		},
		{
			name:     "quad internal type",
			argument: FunctionArgument{Name: "F_QUAD_0", FieldType: sql.NullInt64{Int64: 9, Valid: true}},
		},
		{
			name:     "cstring with null length",
			argument: FunctionArgument{Name: "F_CSTR_0", FieldType: sql.NullInt64{Int64: fieldTypeCString, Valid: true}},
		},
		{
			name: "cstring with zero length",
			argument: FunctionArgument{
				Name:        "F_CSTR_1",
				FieldType:   sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength: sql.NullInt64{Int64: 0, Valid: true},
			},
		},
		{
			name: "cstring under a non-default character set",
			argument: FunctionArgument{
				Name:           "F_CSTR_2",
				FieldType:      sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength:    sql.NullInt64{Int64: 80, Valid: true},
				CharacterSetID: sql.NullInt64{Int64: 4, Valid: true},
			},
		},
		{
			name: "char under a non-default character set",
			argument: FunctionArgument{
				Name:            "F_CHAR_0",
				FieldType:       sql.NullInt64{Int64: fieldTypeChar, Valid: true},
				FieldLength:     sql.NullInt64{Int64: 40, Valid: true},
				CharacterLength: sql.NullInt64{Int64: 10, Valid: true},
				CharacterSetID:  sql.NullInt64{Int64: 4, Valid: true},
			},
		},
		{
			name: "blob under a non-default character set",
			argument: FunctionArgument{
				Name:           "F_BLOB_2",
				FieldType:      sql.NullInt64{Int64: fieldTypeBlob, Valid: true},
				FieldLength:    sql.NullInt64{Int64: 0, Valid: true},
				CharacterSetID: sql.NullInt64{Int64: 4, Valid: true},
			},
		},
		{
			// The real-world CHAR case: RDB$CHARACTER_LENGTH is NULL for every
			// measured function argument, so CHAR and VARCHAR arguments are
			// expected to refuse. Do not substitute FieldLength here.
			name: "char without a character length",
			argument: FunctionArgument{
				Name:        "F_CHAR_1",
				FieldType:   sql.NullInt64{Int64: fieldTypeChar, Valid: true},
				FieldLength: sql.NullInt64{Int64: 40, Valid: true},
			},
		},
		{
			name: "varchar without a character length",
			argument: FunctionArgument{
				Name:        "F_VARCHAR_0",
				FieldType:   sql.NullInt64{Int64: fieldTypeVarchar, Valid: true},
				FieldLength: sql.NullInt64{Int64: 40, Valid: true},
			},
		},
	}
	for _, test := range unsupported {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.argument.SQLType(); !errors.Is(err, ErrUnsupportedDDL) {
				t.Fatalf("SQLType error = %v, want ErrUnsupportedDDL", err)
			}
		})
	}

	// The delegated renderer must not leak the synthetic domain into the
	// caller's error.
	_, err := FunctionArgument{Name: "F_NULL_0"}.SQLType()
	var relabelled *UnsupportedDDLError
	if !errors.As(err, &relabelled) {
		t.Fatalf("delegated error = %v, want *UnsupportedDDLError", err)
	}
	if relabelled.Object != "function argument" || relabelled.Name != "F_NULL_0" {
		t.Fatalf("delegated error = %#v, want the function argument object and name", relabelled)
	}
	if relabelled.Feature != "field type is NULL" {
		t.Fatalf("delegated error feature = %q, want the underlying renderer reason", relabelled.Feature)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=0 go test ./schema/... -count=1 -run '^TestFunctionArgumentSQLTypeRendersExternalDeclarations$' -v`

Expected: FAIL — build error `test.argument.SQLType undefined (type FunctionArgument has no field or method SQLType)`.

- [ ] **Step 3: Implement `SQLType`**

Insert into `schema/ddl.go` immediately before the `GenerateDDL` comment for `Function` (currently line 1213):

```go
// SQLType renders the SQL declaration of an external function argument, for
// example "INTEGER" or "CSTRING(80)". Every type other than CSTRING is
// rendered by the same renderer that serves domains, so the two cannot drift.
// No character-set suffix is ever emitted: RDB$FUNCTION_ARGUMENTS supplies no
// character-set name, so an argument declared under a non-default character
// set returns an error wrapping ErrUnsupportedDDL rather than a declaration
// with the clause silently dropped.
func (a FunctionArgument) SQLType() (string, error) {
	if a.FieldType.Valid && a.FieldType.Int64 == fieldTypeCString {
		// CSTRING declares a byte length, and RDB$CHARACTER_LENGTH is not
		// populated for function arguments; RDB$FIELD_LENGTH is used verbatim.
		if !a.FieldLength.Valid || a.FieldLength.Int64 <= 0 {
			return "", unsupportedDDL("function argument", a.Name, "CSTRING length is unavailable")
		}
		// The name is always invalid here: RDB$FUNCTION_ARGUMENTS supplies no
		// character-set name, so this call can only return "" or the refusal.
		if _, err := characterSetClause("function argument", a.Name, sql.NullString{}, a.CharacterSetID); err != nil {
			return "", err
		}
		return fmt.Sprintf("CSTRING(%d)", a.FieldLength.Int64), nil
	}

	domain := Domain{
		Name:            a.Name,
		FieldType:       a.FieldType,
		FieldSubType:    a.FieldSubType,
		FieldScale:      a.FieldScale,
		FieldLength:     a.FieldLength,
		FieldPrecision:  a.FieldPrecision,
		CharacterLength: a.CharacterLength,
		CharacterSetID:  a.CharacterSetID,
	}
	parts, err := domain.sqlTypeParts()
	if err != nil {
		var unsupported *UnsupportedDDLError
		if errors.As(err, &unsupported) {
			return "", unsupportedDDL("function argument", a.Name, unsupported.Feature)
		}
		return "", err
	}
	return parts.render(true), nil
}
```

`sql`, `errors`, and `fmt` are already imported by `schema/ddl.go` (`ddl.go:3-11`); add no import.

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=0 go test ./schema/... -count=1 -run '^TestFunctionArgumentSQLTypeRendersExternalDeclarations$' -v`

Expected: PASS, including every subtest. If a CHAR or VARCHAR subtest fails because the argument rendered instead of refusing, the implementation substituted `FieldLength` for a missing character length — remove that substitution rather than relaxing the test.

- [ ] **Step 5: Run the whole schema suite and vet**

Run:

```sh
CGO_ENABLED=0 go test ./schema/... -count=1
CGO_ENABLED=0 go vet ./schema/...
```

Expected: `ok  interbase-go/schema` and no vet output.

- [ ] **Step 6: Commit**

```bash
git add schema/ddl.go schema/extended_schema_test.go
git commit -m "Render external function argument declarations through the domain renderer"
```

---

### Task 4: `Function.ReturnType` and fixture-backed coverage

**Files:**
- Modify: `schema/ddl.go` (insert immediately after `FunctionArgument.SQLType` from Task 3)
- Modify: `schema/schema_test.go:1592-1598` (`fixtureFunctions`)
- Test: `schema/extended_schema_test.go`

**Interfaces:**
- Consumes: `FunctionArgument.SQLType() (string, error)` from Task 3, `unsupportedDDL` (`ddl.go:51`), `Function` (`catalog_extended.go:118-127`) with `ReturnArgument sql.NullInt64` and `Arguments []FunctionArgument`, `openFixtureDB(t *testing.T) *sql.DB` (`schema_test.go:763`), `Catalog.Functions(ctx, name) ([]Function, error)` (`catalog_extended.go:855`).
- Produces: `func (f Function) ReturnType() (string, error)` — the `SQLType` of the argument whose `Position` equals `ReturnArgument`.

- [ ] **Step 1: Write the failing test**

Append to `schema/extended_schema_test.go`:

```go
func TestFunctionReturnTypeResolvesByArgumentPosition(t *testing.T) {
	denseReturn := Function{
		Name:           "GO_DENSE",
		ReturnArgument: sql.NullInt64{Int64: 0, Valid: true},
		Arguments: []FunctionArgument{{
			Name:           "GO_DENSE_0",
			Position:       sql.NullInt64{Int64: 0, Valid: true},
			FieldType:      sql.NullInt64{Int64: fieldTypeInteger, Valid: true},
			FieldSubType:   sql.NullInt64{Int64: 0, Valid: true},
			FieldScale:     sql.NullInt64{Int64: 0, Valid: true},
			FieldLength:    sql.NullInt64{Int64: 4, Valid: true},
			FieldPrecision: sql.NullInt64{Int64: 10, Valid: true},
		}},
	}
	if got, err := denseReturn.ReturnType(); err != nil || got != "INTEGER" {
		t.Fatalf("position-0 ReturnType = (%q, %v), want (\"INTEGER\", nil)", got, err)
	}

	// RDB$RETURN_ARGUMENT is a position, not a slice index. Positions here are
	// 1 and 3, so indexing Arguments by ReturnArgument would panic or resolve
	// the wrong row; argument 3 is simultaneously an input and the return.
	sparseReturn := Function{
		Name:           "GO_SPARSE",
		ReturnArgument: sql.NullInt64{Int64: 3, Valid: true},
		Arguments: []FunctionArgument{
			{
				Name:           "GO_SPARSE_1",
				Position:       sql.NullInt64{Int64: 1, Valid: true},
				FieldType:      sql.NullInt64{Int64: fieldTypeInteger, Valid: true},
				FieldSubType:   sql.NullInt64{Int64: 0, Valid: true},
				FieldScale:     sql.NullInt64{Int64: 0, Valid: true},
				FieldLength:    sql.NullInt64{Int64: 4, Valid: true},
				FieldPrecision: sql.NullInt64{Int64: 10, Valid: true},
			},
			{
				Name:        "GO_SPARSE_3",
				Position:    sql.NullInt64{Int64: 3, Valid: true},
				FieldType:   sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength: sql.NullInt64{Int64: 80, Valid: true},
			},
		},
	}
	if got, err := sparseReturn.ReturnType(); err != nil || got != "CSTRING(80)" {
		t.Fatalf("positional ReturnType = (%q, %v), want (\"CSTRING(80)\", nil)", got, err)
	}

	unsupported := []struct {
		name     string
		function Function
	}{
		{
			name:     "null return argument",
			function: Function{Name: "GO_NULL_RETURN", Arguments: denseReturn.Arguments},
		},
		{
			name: "return position absent",
			function: Function{
				Name:           "GO_MISSING_RETURN",
				ReturnArgument: sql.NullInt64{Int64: 5, Valid: true},
				Arguments:      denseReturn.Arguments,
			},
		},
	}
	for _, test := range unsupported {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.function.ReturnType(); !errors.Is(err, ErrUnsupportedDDL) {
				t.Fatalf("ReturnType error = %v, want ErrUnsupportedDDL", err)
			}
		})
	}

	db := openFixtureDB(t)
	defer db.Close()
	catalog := New(db)
	ctx := context.Background()

	scanned, err := catalog.Functions(ctx, "GO_EXTERNAL")
	if err != nil {
		t.Fatalf("Functions returned error: %v", err)
	}
	if len(scanned) != 1 || len(scanned[0].Arguments) != 1 {
		t.Fatalf("scanned function = %#v, want one function with one argument", scanned)
	}
	if got, err := scanned[0].Arguments[0].SQLType(); err != nil || got != "INTEGER" {
		t.Fatalf("scanned argument SQLType = (%q, %v), want (\"INTEGER\", nil)", got, err)
	}
	if got, err := scanned[0].ReturnType(); err != nil || got != "INTEGER" {
		t.Fatalf("scanned ReturnType = (%q, %v), want (\"INTEGER\", nil)", got, err)
	}

	text, err := catalog.Functions(ctx, "GO_EXTERNAL_TEXT")
	if err != nil {
		t.Fatalf("Functions returned error: %v", err)
	}
	if len(text) != 1 || len(text[0].Arguments) != 1 {
		t.Fatalf("scanned CSTRING function = %#v, want one function with one argument", text)
	}
	if got, err := text[0].Arguments[0].SQLType(); err != nil || got != "CSTRING(80)" {
		t.Fatalf("scanned CSTRING argument SQLType = (%q, %v), want (\"CSTRING(80)\", nil)", got, err)
	}
	if got, err := text[0].ReturnType(); err != nil || got != "CSTRING(80)" {
		t.Fatalf("scanned CSTRING ReturnType = (%q, %v), want (\"CSTRING(80)\", nil)", got, err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=0 go test ./schema/... -count=1 -run '^TestFunctionReturnTypeResolvesByArgumentPosition$' -v`

Expected: FAIL — build error `denseReturn.ReturnType undefined (type Function has no field or method ReturnType)`.

- [ ] **Step 3: Implement `ReturnType`**

Insert into `schema/ddl.go` immediately after `FunctionArgument.SQLType`:

```go
// ReturnType renders the SQL declaration of the function's return value.
// RDB$RETURN_ARGUMENT is an argument position, not an index into Arguments:
// when it is N > 0 the return value is argument N, which is simultaneously an
// input. Catalog positions are unique per function, so the first match wins.
func (f Function) ReturnType() (string, error) {
	if !f.ReturnArgument.Valid {
		return "", unsupportedDDL("external function", f.Name, "return argument position is NULL")
	}
	for _, argument := range f.Arguments {
		if argument.Position.Valid && argument.Position.Int64 == f.ReturnArgument.Int64 {
			return argument.SQLType()
		}
	}
	return "", unsupportedDDL("external function", f.Name, fmt.Sprintf("no argument at return position %d", f.ReturnArgument.Int64))
}
```

- [ ] **Step 4: Add the CSTRING fixture function**

The test needs a scanned CSTRING argument. Add a second entry to `fixtureFunctions` in `schema/schema_test.go:1592-1598` rather than adding a row to `GO_EXTERNAL`, so the existing argument-count assertion at `extended_schema_test.go:91-93` keeps holding unchanged. Replace:

```go
func fixtureFunctions() []fixtureFunction {
	return []fixtureFunction{{
		name:      "GO_EXTERNAL",
		values:    []driver.Value{"GO_EXTERNAL      ", int64(0), nil, "go_udf.so       ", "go_external     ", int64(0), int64(0)},
		arguments: [][]driver.Value{{"GO_EXTERNAL      ", int64(0), int64(0), int64(4), int64(0), int64(8), int64(0), nil, int64(10), nil}},
	}}
}
```

with:

```go
func fixtureFunctions() []fixtureFunction {
	return []fixtureFunction{
		{
			name:      "GO_EXTERNAL",
			values:    []driver.Value{"GO_EXTERNAL      ", int64(0), nil, "go_udf.so       ", "go_external     ", int64(0), int64(0)},
			arguments: [][]driver.Value{{"GO_EXTERNAL      ", int64(0), int64(0), int64(4), int64(0), int64(8), int64(0), nil, int64(10), nil}},
		},
		{
			// A CSTRING(80) return argument with RDB$CHARACTER_LENGTH NULL and
			// RDB$FIELD_LENGTH 80, matching every measured catalog row.
			name:      "GO_EXTERNAL_TEXT",
			values:    []driver.Value{"GO_EXTERNAL_TEXT ", int64(0), nil, "go_udf.so       ", "go_external_text", int64(0), int64(0)},
			arguments: [][]driver.Value{{"GO_EXTERNAL_TEXT ", int64(0), int64(1), int64(80), int64(0), int64(40), nil, nil, nil, nil}},
		},
	}
}
```

The argument row follows `functionArgumentsQuery`'s projection order (`catalog_extended.go:263-270`): function name, argument position, mechanism, field length, field scale, field type, field sub-type, character set id, field precision, character length.

- [ ] **Step 5: Run the test to verify it passes**

Run: `CGO_ENABLED=0 go test ./schema/... -count=1 -run '^TestFunctionReturnTypeResolvesByArgumentPosition$' -v`

Expected: PASS. A panic with `index out of range [3] with length 2` means the implementation indexed `Arguments` by `ReturnArgument` instead of matching `Position`.

- [ ] **Step 6: Run the whole schema suite and vet**

Run:

```sh
CGO_ENABLED=0 go test ./schema/... -count=1
CGO_ENABLED=0 go vet ./schema/...
```

Expected: `ok  interbase-go/schema` and no vet output. `TestCatalogReadsExtendedSchemaFamilies` must still pass unchanged — it filters on `GO_EXTERNAL` exactly, so the new fixture function is invisible to it.

- [ ] **Step 7: Commit**

```bash
git add schema/ddl.go schema/schema_test.go schema/extended_schema_test.go
git commit -m "Resolve external function return type by argument position"
```

---

### Task 5: Live acceptance coverage and README

**Files:**
- Modify: `integration/schema_test.go:228-261` (the `createSchemaObjects` list), `integration/schema_test.go:356-370` (trigger assertions), `integration/schema_test.go:415-424` (function assertions)
- Modify: `schema/README.md:43-57` (*Extended catalog objects*)

**Interfaces:**
- Consumes: `Trigger.Event`, `FunctionArgument.SQLType`, and `Function.ReturnType` from Tasks 2-4, reached as `catalogschema` (`integration/schema_test.go:14`); `catalog.Trigger(ctx, name)`, `catalog.Function(ctx, name)`.
- Produces: no new interface. This task pins the `CSTRING(80)` rule against a real InterBase catalog and documents the three accessors.

- [ ] **Step 1: Add the CSTRING declaration to the live fixture**

In `TestSchemaExtendedCatalogFamiliesAndDDL`, insert into the `createSchemaObjects` argument list directly after the existing `GO_SCHEMA_EXT_FUNCTION` declaration (`integration/schema_test.go:258`):

```go
		`DECLARE EXTERNAL FUNCTION GO_SCHEMA_EXT_CSTRING CSTRING(80)
RETURNS CSTRING(80) FREE_IT
ENTRY_POINT 'go_schema_ext_cstring' MODULE_NAME 'go_schema_ext_library'`,
```

Declaring an external function records metadata only; no module is loaded and no UDF is invoked, exactly as for the declaration already on line 258.

Distinguish two failure modes here, because they are easy to confuse and call
for opposite responses. If the server *rejects this DDL*, `createSchemaObjects`
fails and the whole pre-existing `TestSchemaExtendedCatalogFamiliesAndDDL` goes
red rather than only the new assertions — that is a fixture problem, so fix the
declaration. The "revisit the column choice, never add a +/-1" instruction in
Step 3 applies only when the DDL is accepted and `CSTRING(80)` still renders
with the wrong length.

- [ ] **Step 2: Assert the trigger event**

After the existing trigger DDL assertion block that ends at `integration/schema_test.go:370`, add:

```go
	if event, err := trigger.Event(); err != nil || event != "BEFORE INSERT" {
		t.Fatalf("trigger Event = (%q, %v), want (\"BEFORE INSERT\", nil)", event, err)
	}
```

`GO_SCHEMA_EXT_TRIGGER` is declared `ACTIVE BEFORE INSERT` (`integration/schema_test.go:252-253`).

- [ ] **Step 3: Assert the function return and argument types**

After the existing external-function block that ends at `integration/schema_test.go:424`, add:

```go
	if returnType, err := function.ReturnType(); err != nil || returnType != "INTEGER" {
		t.Fatalf("external function ReturnType = (%q, %v), want (\"INTEGER\", nil)", returnType, err)
	}
	checkedArgument := false
	for _, argument := range function.Arguments {
		if !argument.Position.Valid || argument.Position.Int64 != 0 {
			continue
		}
		checkedArgument = true
		if sqlType, err := argument.SQLType(); err != nil || sqlType != "INTEGER" {
			t.Fatalf("external function argument 0 SQLType = (%q, %v), want (\"INTEGER\", nil)", sqlType, err)
		}
	}
	// Without this the loop passes vacuously when the catalog places no
	// argument at position 0, and the assertion above never runs.
	if !checkedArgument {
		t.Fatalf("external function arguments = %#v, want one at position 0", function.Arguments)
	}

	cstringFunction, err := catalog.Function(ctx, "GO_SCHEMA_EXT_CSTRING")
	if err != nil {
		t.Fatalf("CSTRING external function lookup: %v", err)
	}
	if cstringFunction == nil || len(cstringFunction.Arguments) == 0 {
		t.Fatalf("CSTRING external function = %#v, want declared argument metadata", cstringFunction)
	}
	// The declared length is CSTRING(80). A mismatch here means the rule reads
	// the wrong catalog column; fix the column choice, never add a +/-1.
	if returnType, err := cstringFunction.ReturnType(); err != nil || returnType != "CSTRING(80)" {
		t.Fatalf("CSTRING ReturnType = (%q, %v), want (\"CSTRING(80)\", nil)", returnType, err)
	}
	for _, argument := range cstringFunction.Arguments {
		sqlType, err := argument.SQLType()
		if err != nil || sqlType != "CSTRING(80)" {
			t.Fatalf("CSTRING argument %d SQLType = (%q, %v), want (\"CSTRING(80)\", nil)", argument.Position.Int64, sqlType, err)
		}
	}
```

- [ ] **Step 4: Compile-check the integration suite**

Run: `go vet -tags integration ./integration/...`

Expected: no output. This validates the new live code compiles without a server; it is the gate this task can always run.

- [ ] **Step 5: Run the live test**

Run:

```sh
make test-integration-docker GO_TEST_ARGS="-run '^TestSchemaExtendedCatalogFamiliesAndDDL$' -v"
```

Expected: PASS. If the Docker fixture, image, or InterBase SDK is unavailable in this environment, record that the live assertions were compile-checked only and were not executed — do not report them as passing.

- [ ] **Step 6: Document the accessors**

In `schema/README.md`, insert this paragraph under *Extended catalog objects*, after the paragraph ending "no UDF is ever loaded or called by this package." (`schema/README.md:55-57`):

```markdown
`Trigger.Event` decodes `RDB$TRIGGERS.RDB$TRIGGER_TYPE` into its readable
form, including database-level events such as `ON CONNECT` and multi-event
DML forms such as `BEFORE INSERT OR UPDATE`, which remain valid metadata even
where InterBase trigger DDL cannot express them. `FunctionArgument.SQLType`
and `Function.ReturnType` render external-argument declarations;
`Function.ReturnType` treats `RDB$RETURN_ARGUMENT` as an argument position,
not as an index. Neither emits a character-set suffix, because
`RDB$FUNCTION_ARGUMENTS` supplies no character-set name, and both refuse an
argument declared under a non-default character set rather than dropping the
clause. `CSTRING` lengths come from `RDB$FIELD_LENGTH` unadjusted, while
`CHAR` and `VARCHAR` arguments are not renderable because the catalog supplies
no character length for them. Unsupported metadata continues to return an
error wrapping `ErrUnsupportedDDL`.
```

- [ ] **Step 7: Run the full unit gate one last time**

Run:

```sh
CGO_ENABLED=0 go test ./schema/... -count=1
CGO_ENABLED=0 go vet ./schema/...
go vet -tags integration ./integration/...
```

Expected: `ok  interbase-go/schema`, no vet output from either vet command.

- [ ] **Step 8: Commit**

```bash
git add integration/schema_test.go schema/README.md
git commit -m "Pin catalog accessor rendering against a live InterBase catalog"
```
