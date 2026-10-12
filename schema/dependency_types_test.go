package schema

import "testing"

func TestDependencyObjectTypeNames(t *testing.T) {
	for _, tc := range []struct {
		code  DependencyObjectType
		name  string
		known bool
	}{
		{DependencyRelation, "relation", true},
		{DependencyView, "view", true},
		{DependencyTrigger, "trigger", true},
		{DependencyComputedField, "computed field", true},
		{DependencyProcedure, "procedure", true},
		{DependencyException, "exception", true},
		{DependencyGenerator, "generator", true},
		{DependencyFunction, "function", true},
		{DependencyObjectType(4), "type 4", false},
		{DependencyObjectType(99), "type 99", false},
	} {
		if got := tc.code.String(); got != tc.name {
			t.Errorf("%d.String() = %q, want %q", int64(tc.code), got, tc.name)
		}
		if got := tc.code.Known(); got != tc.known {
			t.Errorf("%d.Known() = %v, want %v", int64(tc.code), got, tc.known)
		}
	}
}
