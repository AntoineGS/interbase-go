package schema

import (
	"fmt"
	"sort"
	"strings"
)

// This explicit profile targets the InterBase catalog used by the current
// loader. Extra engine columns are not automatically selected. Every required
// column must exist, including on an empty family, or the watcher is unsupported.
const fingerprintVersion = "interbase-15-catalog-v1"

// SystemFlag routing retention does not change this projection/key/digest
// profile. Fingerprints are in-process values, not persisted encodings; no
// format bump is needed for retaining a value already present in the digest.

type fingerprintProjection struct {
	Relation                                    string
	KeyColumns, ValueColumns, IdentifierColumns []string
}

func projection(relation, keys, values, identifiers string) fingerprintProjection {
	cols := func(s string) []string {
		if s == "" {
			return nil
		}
		r := strings.Fields(s)
		for i := range r {
			r[i] = "RDB$" + r[i]
		}
		return r
	}
	return fingerprintProjection{"RDB$" + relation, cols(keys), cols(values), cols(identifiers)}
}
func (p fingerprintProjection) columns() []string {
	return append(append([]string(nil), p.KeyColumns...), p.ValueColumns...)
}
func fingerprintManifest() []fingerprintProjection {
	return []fingerprintProjection{
		projection("RELATIONS", "RELATION_NAME", "RELATION_ID RELATION_TYPE DBKEY_LENGTH FORMAT OWNER_NAME SECURITY_CLASS DEFAULT_CLASS FLAGS SYSTEM_FLAG EXTERNAL_FILE EXTERNAL_DESCRIPTION VIEW_SOURCE VIEW_BLR DESCRIPTION", "RELATION_NAME OWNER_NAME SECURITY_CLASS DEFAULT_CLASS"),
		projection("RELATION_FIELDS", "RELATION_NAME FIELD_NAME", "FIELD_SOURCE FIELD_ID FIELD_POSITION UPDATE_FLAG NULL_FLAG SYSTEM_FLAG SECURITY_CLASS DEFAULT_VALUE DEFAULT_SOURCE DESCRIPTION COLLATION_ID VIEW_CONTEXT BASE_FIELD", "RELATION_NAME FIELD_NAME FIELD_SOURCE SECURITY_CLASS BASE_FIELD"),
		projection("VIEW_RELATIONS", "VIEW_NAME VIEW_CONTEXT RELATION_NAME", "CONTEXT_NAME", "VIEW_NAME RELATION_NAME CONTEXT_NAME"),
		projection("FIELDS", "FIELD_NAME", "FIELD_TYPE FIELD_SUB_TYPE FIELD_LENGTH FIELD_SCALE FIELD_PRECISION NULL_FLAG CHARACTER_SET_ID COLLATION_ID CHARACTER_LENGTH SEGMENT_LENGTH DIMENSIONS EXTERNAL_LENGTH EXTERNAL_SCALE EXTERNAL_TYPE SYSTEM_FLAG DEFAULT_VALUE DEFAULT_SOURCE COMPUTED_BLR COMPUTED_SOURCE VALIDATION_BLR VALIDATION_SOURCE DESCRIPTION", "FIELD_NAME"),
		projection("FIELD_DIMENSIONS", "FIELD_NAME DIMENSION", "LOWER_BOUND UPPER_BOUND", "FIELD_NAME"),
		projection("PROCEDURES", "PROCEDURE_NAME", "PROCEDURE_ID PROCEDURE_INPUTS PROCEDURE_OUTPUTS OWNER_NAME SECURITY_CLASS SYSTEM_FLAG PROCEDURE_SOURCE PROCEDURE_BLR DESCRIPTION", "PROCEDURE_NAME OWNER_NAME SECURITY_CLASS"),
		projection("PROCEDURE_PARAMETERS", "PROCEDURE_NAME PARAMETER_TYPE PARAMETER_NUMBER PARAMETER_NAME", "FIELD_SOURCE DESCRIPTION SYSTEM_FLAG", "PROCEDURE_NAME PARAMETER_NAME FIELD_SOURCE"),
		projection("TRIGGERS", "TRIGGER_NAME", "RELATION_NAME TRIGGER_TYPE TRIGGER_SEQUENCE TRIGGER_INACTIVE SYSTEM_FLAG FLAGS TRIGGER_SOURCE TRIGGER_BLR DESCRIPTION", "TRIGGER_NAME RELATION_NAME"),
		projection("TRIGGER_MESSAGES", "TRIGGER_NAME MESSAGE_NUMBER", "MESSAGE", "TRIGGER_NAME"),
		projection("INDICES", "INDEX_NAME", "RELATION_NAME INDEX_ID UNIQUE_FLAG INDEX_INACTIVE INDEX_TYPE SYSTEM_FLAG SEGMENT_COUNT FOREIGN_KEY EXPRESSION_SOURCE EXPRESSION_BLR DESCRIPTION", "INDEX_NAME RELATION_NAME FOREIGN_KEY"),
		projection("INDEX_SEGMENTS", "INDEX_NAME FIELD_POSITION", "FIELD_NAME", "INDEX_NAME FIELD_NAME"),
		projection("RELATION_CONSTRAINTS", "CONSTRAINT_NAME", "RELATION_NAME CONSTRAINT_TYPE INDEX_NAME DEFERRABLE INITIALLY_DEFERRED", "CONSTRAINT_NAME RELATION_NAME INDEX_NAME"),
		projection("REF_CONSTRAINTS", "CONSTRAINT_NAME", "CONST_NAME_UQ MATCH_OPTION UPDATE_RULE DELETE_RULE", "CONSTRAINT_NAME CONST_NAME_UQ"),
		projection("CHECK_CONSTRAINTS", "CONSTRAINT_NAME TRIGGER_NAME", "", "CONSTRAINT_NAME TRIGGER_NAME"),
		projection("GENERATORS", "GENERATOR_NAME", "GENERATOR_ID SYSTEM_FLAG DESCRIPTION", "GENERATOR_NAME"),
		projection("EXCEPTIONS", "EXCEPTION_NAME", "EXCEPTION_NUMBER MESSAGE DESCRIPTION SYSTEM_FLAG", "EXCEPTION_NAME"),
		projection("FUNCTIONS", "FUNCTION_NAME", "FUNCTION_TYPE MODULE_NAME ENTRYPOINT RETURN_ARGUMENT SYSTEM_FLAG DESCRIPTION", "FUNCTION_NAME"),
		projection("FUNCTION_ARGUMENTS", "FUNCTION_NAME ARGUMENT_POSITION", "MECHANISM FIELD_TYPE FIELD_SUB_TYPE FIELD_LENGTH FIELD_SCALE CHARACTER_SET_ID FIELD_PRECISION CHARACTER_LENGTH", "FUNCTION_NAME"),
		projection("DEPENDENCIES", "DEPENDENT_NAME DEPENDENT_TYPE DEPENDED_ON_NAME DEPENDED_ON_TYPE FIELD_NAME", "", "DEPENDENT_NAME DEPENDED_ON_NAME FIELD_NAME"),
		projection("CHARACTER_SETS", "CHARACTER_SET_ID", "CHARACTER_SET_NAME DEFAULT_COLLATE_NAME BYTES_PER_CHARACTER NUMBER_OF_CHARACTERS SYSTEM_FLAG DESCRIPTION FUNCTION_NAME", "CHARACTER_SET_NAME DEFAULT_COLLATE_NAME FUNCTION_NAME"),
		projection("COLLATIONS", "CHARACTER_SET_ID COLLATION_ID", "COLLATION_NAME COLLATION_ATTRIBUTES SYSTEM_FLAG DESCRIPTION FUNCTION_NAME", "COLLATION_NAME FUNCTION_NAME"),
		projection("USER_PRIVILEGES", "USER GRANTOR PRIVILEGE GRANT_OPTION RELATION_NAME FIELD_NAME USER_TYPE OBJECT_TYPE", "", "USER GRANTOR RELATION_NAME FIELD_NAME"),
		projection("ROLES", "ROLE_NAME", "OWNER_NAME DESCRIPTION", "ROLE_NAME OWNER_NAME"),
		projection("SECURITY_CLASSES", "SECURITY_CLASS", "ACL DESCRIPTION", "SECURITY_CLASS"),
		projection("DATABASE", "", "CHARACTER_SET_NAME SECURITY_CLASS DESCRIPTION", "CHARACTER_SET_NAME SECURITY_CLASS"),
	}
}
func fingerprintFamilies() []string {
	var r []string
	for _, p := range fingerprintManifest() {
		r = append(r, p.Relation)
	}
	sort.Strings(r)
	return r
}
func fingerprintQueries(widths map[identifierField]int) ([]CatalogFingerprintQuery, error) {
	var queries []CatalogFingerprintQuery
	for _, p := range fingerprintManifest() {
		var selectCols []string
		for _, c := range p.columns() {
			w, ok := widths[identifierField{p.Relation, c}]
			if !ok {
				return nil, fmt.Errorf("%w: profile %s requires %s.%s", ErrFingerprintCapability, fingerprintVersion, p.Relation, c)
			}
			ref := c
			for _, id := range p.IdentifierColumns {
				if c == id {
					var err error
					ref, err = catalogIdentifier(c, w)
					if err != nil {
						return nil, fmt.Errorf("%w: invalid width for %s.%s", ErrFingerprintCapability, p.Relation, c)
					}
					ref += " AS " + c
					break
				}
			}
			selectCols = append(selectCols, ref)
		}
		queries = append(queries, CatalogFingerprintQuery{SQL: "SELECT " + strings.Join(selectCols, ", ") + " FROM " + p.Relation})
	}
	return queries, nil
}
