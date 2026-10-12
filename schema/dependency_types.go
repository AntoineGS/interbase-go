package schema

import "fmt"

// DependencyObjectType is an RDB$DEPENDENCIES object-type code. InterBase's
// codes differ from Firebird's: generators are 11 and UDFs 14. Only codes
// verified against a live catalog are named; others render as "type N".
type DependencyObjectType int64

const (
	DependencyRelation      DependencyObjectType = 0
	DependencyView          DependencyObjectType = 1
	DependencyTrigger       DependencyObjectType = 2
	DependencyComputedField DependencyObjectType = 3
	DependencyProcedure     DependencyObjectType = 5
	DependencyException     DependencyObjectType = 7
	DependencyGenerator     DependencyObjectType = 11
	DependencyFunction      DependencyObjectType = 14
)

var dependencyObjectTypeNames = map[DependencyObjectType]string{
	DependencyRelation:      "relation",
	DependencyView:          "view",
	DependencyTrigger:       "trigger",
	DependencyComputedField: "computed field",
	DependencyProcedure:     "procedure",
	DependencyException:     "exception",
	DependencyGenerator:     "generator",
	DependencyFunction:      "function",
}

func (t DependencyObjectType) String() string {
	if name, ok := dependencyObjectTypeNames[t]; ok {
		return name
	}
	return fmt.Sprintf("type %d", int64(t))
}

// Known reports whether t is one of the verified codes.
func (t DependencyObjectType) Known() bool {
	_, ok := dependencyObjectTypeNames[t]
	return ok
}
