package schema

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCatalogIdentifierProjectionsUseCatalogByteWidths(t *testing.T) {
	db, state := openFixtureDBWithOptions(t, fixtureOptions{})
	defer db.Close()
	catalog := New(db)

	widths, err := catalog.identifierWidths(context.Background())
	if err != nil {
		t.Fatalf("identifierWidths returned error: %v", err)
	}
	if got := widths[identifierField{relation: "RDB$RELATIONS", field: "RDB$RELATION_NAME"}]; got != 67 {
		t.Fatalf("relation-name width = %d, want 67 bytes", got)
	}
	if got := widths[identifierField{relation: "RDB$RELATION_FIELDS", field: "RDB$RELATION_NAME"}]; got != 31 {
		t.Fatalf("relation-field relation-name width = %d, want 31 bytes", got)
	}
	if got := widths[identifierField{relation: "RDB$RELATION_FIELDS", field: "RDB$FIELD_NAME"}]; got != 31 {
		t.Fatalf("field-name width = %d, want 31 bytes", got)
	}

	const longRelation = "RDB$RELATION_CONSTRAINTS"
	if len(longRelation) <= 22 {
		t.Fatalf("test relation %q is not longer than the old 22-character limit", longRelation)
	}
	if got := widths[identifierField{relation: longRelation, field: "RDB$CONSTRAINT_NAME"}]; got != 31 {
		t.Fatalf("long relation key width = %d, want 31", got)
	}

	relationProjection, err := catalogIdentifier("r.RDB$RELATION_NAME", widths[identifierField{relation: "RDB$RELATIONS", field: "RDB$RELATION_NAME"}])
	if err != nil {
		t.Fatalf("catalogIdentifier for relation name returned error: %v", err)
	}
	fieldProjection, err := catalogIdentifier("rf.RDB$FIELD_NAME", widths[identifierField{relation: "RDB$RELATION_FIELDS", field: "RDB$FIELD_NAME"}])
	if err != nil {
		t.Fatalf("catalogIdentifier for field name returned error: %v", err)
	}
	if want := `CAST(r.RDB$RELATION_NAME AS VARCHAR(67))`; relationProjection != want {
		t.Fatalf("relation projection = %q, want %q", relationProjection, want)
	}
	if want := `CAST(rf.RDB$FIELD_NAME AS VARCHAR(31))`; fieldProjection != want {
		t.Fatalf("field projection = %q, want %q", fieldProjection, want)
	}
	if relationProjection == fieldProjection {
		t.Fatalf("relation and field projections unexpectedly match: %q", relationProjection)
	}

	missingWidth := widths[identifierField{relation: "RDB$MISSING_RELATION", field: "RDB$MISSING_FIELD"}]
	if _, err := catalogIdentifier("rf.RDB$FIELD_NAME", missingWidth); err == nil {
		t.Fatal("catalogIdentifier accepted an absent identifierField width")
	}
	if _, err := catalogIdentifier("rf.RDB$FIELD_NAME", 0); err == nil {
		t.Fatal("catalogIdentifier accepted a non-positive width")
	}

	columnQuery := normalizeFixtureSQL(relationColumnsQueryTemplate)
	if !strings.Contains(columnQuery, "WHERE RF.RDB$RELATION_NAME = ?") {
		t.Fatalf("column query filter = %q, want the original uncast catalog predicate", columnQuery)
	}
	if strings.Contains(columnQuery, "WHERE CAST(") {
		t.Fatalf("column query casts its filter instead of only projecting returned values: %q", columnQuery)
	}

	if _, err := catalog.identifierWidths(context.Background()); err != nil {
		t.Fatalf("cached identifierWidths returned error: %v", err)
	}
	calls := state.callsSnapshot()
	if len(calls) != 3 {
		t.Fatalf("identifier width query count after cached read = %d, want 3", len(calls))
	}
	var bootstrappedRelationFieldWidth bool
	var projectedRelationFieldWidth bool
	for _, call := range calls {
		if fixtureQueryKind(call.query) == "identifier_width_bootstrap" && len(call.args) == 2 &&
			call.args[0].Value == "RDB$RELATION_FIELDS" && call.args[1].Value == "RDB$RELATION_NAME" {
			bootstrappedRelationFieldWidth = true
		}
		if fixtureQueryKind(call.query) == "identifier_widths" && strings.Contains(normalizeFixtureSQL(call.query), "CAST(RF.RDB$RELATION_NAME AS VARCHAR(31))") {
			projectedRelationFieldWidth = true
		}
	}
	if !bootstrappedRelationFieldWidth {
		t.Fatal("identifier width bootstrap did not use RDB$RELATION_FIELDS.RDB$RELATION_NAME")
	}
	if !projectedRelationFieldWidth {
		t.Fatal("identifier widths query did not project RDB$RELATION_FIELDS.RDB$RELATION_NAME at its 31-byte width")
	}
}

func TestRelationColumnsProjectRelationNameWithSourceWidth(t *testing.T) {
	db := openFixtureDB(t)
	defer db.Close()

	query, err := New(db).relationColumnsQuery(context.Background())
	if err != nil {
		t.Fatalf("relationColumnsQuery returned error: %v", err)
	}
	if want := "CAST(RF.RDB$RELATION_NAME AS VARCHAR(31)) AS RDB$RELATION_NAME"; !strings.Contains(normalizeFixtureSQL(query), want) {
		t.Fatalf("relation-column relation-name projection = %q, want %q", query, want)
	}
}

func TestCatalogIdentifierWidthsReturnsIsolatedCopy(t *testing.T) {
	db := openFixtureDB(t)
	defer db.Close()
	catalog := New(db)

	widths, err := catalog.identifierWidths(context.Background())
	if err != nil {
		t.Fatalf("identifierWidths returned error: %v", err)
	}
	key := identifierField{relation: "RDB$RELATIONS", field: "RDB$RELATION_NAME"}
	widths[key] = 1

	got, err := catalog.identifierWidths(context.Background())
	if err != nil {
		t.Fatalf("cached identifierWidths returned error: %v", err)
	}
	if got[key] != 67 {
		t.Fatalf("cached relation-name width after caller mutation = %d, want 67", got[key])
	}
}

func TestCatalogIdentifierWidthsRejectInvalidAndUnsupportedWidths(t *testing.T) {
	queryErr := errors.New("identifier width metadata query failed")
	tests := []struct {
		name      string
		options   fixtureOptions
		wantError string
		wantCause error
	}{
		{name: "zero width", options: fixtureOptions{identifierWidthMode: "zero"}, wantError: "width 0"},
		{name: "NULL width", options: fixtureOptions{identifierWidthMode: "null"}, wantError: "NULL"},
		{name: "database rejects unsupported cast", options: fixtureOptions{identifierWidthMode: "unsupported"}, wantError: "database rejected VARCHAR width"},
		{name: "catalog query error", options: fixtureOptions{queryErrorFor: "identifier_widths", queryError: queryErr}, wantError: queryErr.Error(), wantCause: queryErr},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, _ := openFixtureDBWithOptions(t, test.options)
			defer db.Close()

			widths, err := New(db).identifierWidths(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("identifierWidths result = (%v, %v), want error containing %q", widths, err, test.wantError)
			}
			if len(widths) != 0 {
				t.Fatalf("identifierWidths returned partial metadata after failure: %#v", widths)
			}
			if test.wantCause != nil && !errors.Is(err, test.wantCause) {
				t.Fatalf("identifierWidths error = %v, want wrapped cause %v", err, test.wantCause)
			}
		})
	}
}
