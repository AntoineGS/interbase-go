package schema

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unsafe"
)

// CatalogBlobIdentity is a copied native identifier, not a BLOB handle or content.
type CatalogBlobIdentity struct {
	High int32
	Low  uint32
}
type CatalogFingerprintStats struct{ Queries, Rows, BlobOpens, SourceBytes int64 }
type CatalogFingerprintQuery struct {
	SQL  string
	Args []any
}
type CatalogFingerprintVisitor func(query int, columns []string, values []any) error
type CatalogFingerprintReader func(context.Context, *sql.Conn, []CatalogFingerprintQuery, CatalogFingerprintVisitor) (CatalogFingerprintStats, error)
type CatalogObject struct {
	// NativeType preserves dependency/grant object codes without guessing an
	// unknown engine type. Parent scopes view contexts, columns and collations.
	Kind, Name, Parent string
	NativeType         sql.NullInt64
}
type CatalogLink struct {
	From, To CatalogObject
	Field    string
}
type FingerprintRecord struct {
	// Key is hex of the complete typed length-prefixed identity tuple, not a
	// display-normalized name. Dependency/grant keys include every tuple member.
	Family, Key string
	Digest      [32]byte
	Count       uint32
	// SystemFlag retains the raw nullable header/row flag when the existing
	// family projection contains SYSTEM_FLAG. Family defines applicability.
	SystemFlag sql.NullInt64
	Owners     []CatalogObject
	Links      []CatalogLink
}
type CatalogFingerprint struct {
	Version        string
	AccountedBytes int64
	Stats          CatalogFingerprintStats
	storage        fingerprintStorage
}

var (
	ErrFingerprintLimit      = errors.New("schema: fingerprint retained-byte limit")
	ErrFingerprintCapability = errors.New("schema: fingerprint capability unavailable")
)

// fingerprintDigest streams typed, length-prefixed tokens; no formatted values
// or materialized source data become part of the retained fingerprint.
func fingerprintDigest(values []any) ([32]byte, error) {
	h := sha256.New()
	for _, v := range values {
		if err := fingerprintToken(h, v); err != nil {
			return [32]byte{}, err
		}
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d, nil
}

// Equal ignores accounting, timing and scan instrumentation.
func (f *CatalogFingerprint) Equal(other *CatalogFingerprint) bool {
	if f == nil || other == nil {
		return f == other
	}
	if f.Version != other.Version || f.RecordCount() != other.RecordCount() {
		return false
	}
	for i := uint32(0); i < f.storage.entries.n; i++ {
		r := f.storage.record(i)
		s, ok := other.LookupRecord(r.Family, r.Key)
		if !ok || r.Digest != s.Digest || r.Count != s.Count {
			return false
		}
	}
	return true
}

func addFingerprintStats(a *CatalogFingerprintStats, b CatalogFingerprintStats) {
	a.Queries += b.Queries
	a.Rows += b.Rows
	a.BlobOpens += b.BlobOpens
	a.SourceBytes += b.SourceBytes
}

// ReadCatalogFingerprint discovers widths without reading catalog BLOB content,
// then makes one streaming projection pass. Discovery and the pass each finish
// their short read-only transaction; no post-read rescan is performed.
func ReadCatalogFingerprint(ctx context.Context, conn *sql.Conn, limit int64, reader CatalogFingerprintReader) (*CatalogFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, ErrFingerprintCapability
	}
	if limit <= 0 {
		return nil, ErrFingerprintLimit
	}
	f := &CatalogFingerprint{Version: fingerprintVersion}
	bootstrap := []CatalogFingerprintQuery{{SQL: catalogIdentifierFieldWidthQuery, Args: []any{"RDB$RELATION_FIELDS", "RDB$RELATION_NAME"}}, {SQL: catalogIdentifierFieldWidthQuery, Args: []any{"RDB$RELATION_FIELDS", "RDB$FIELD_NAME"}}}
	var initial [2]int
	var seen [2]bool
	stats, err := reader(ctx, conn, bootstrap, func(q int, _ []string, v []any) error {
		if q < 0 || q >= 2 || seen[q] || len(v) != 1 {
			return ErrFingerprintCapability
		}
		n, ok := v[0].(int64)
		if !ok {
			return ErrFingerprintCapability
		}
		w, e := checkedCatalogWidth(sql.NullInt64{Int64: n, Valid: true}, "fingerprint bootstrap")
		if e != nil {
			return ErrFingerprintCapability
		}
		initial[q] = w
		seen[q] = true
		return nil
	})
	addFingerprintStats(&f.Stats, stats)
	if err != nil {
		return nil, err
	}
	if !seen[0] || !seen[1] {
		return nil, ErrFingerprintCapability
	}
	relation, _ := catalogIdentifier("rf.RDB$RELATION_NAME", initial[0])
	field, _ := catalogIdentifier("rf.RDB$FIELD_NAME", initial[1])
	query := "SELECT " + relation + ", " + field + ", f.RDB$FIELD_LENGTH FROM RDB$RELATION_FIELDS rf JOIN RDB$FIELDS f ON rf.RDB$FIELD_SOURCE = f.RDB$FIELD_NAME"
	widths := map[identifierField]int{}
	// This bounded transient capability table is not retained in the result.
	// It only admits manifest columns, not arbitrary catalog/user definitions.
	required := map[identifierField]bool{}
	for _, p := range fingerprintManifest() {
		for _, c := range p.columns() {
			required[identifierField{p.Relation, c}] = true
		}
	}
	stats, err = reader(ctx, conn, []CatalogFingerprintQuery{{SQL: query}}, func(_ int, _ []string, v []any) error {
		if len(v) != 3 {
			return ErrFingerprintCapability
		}
		r, rok := v[0].(string)
		c, cok := v[1].(string)
		n, nok := v[2].(int64)
		if !rok || !cok || !nok {
			return ErrFingerprintCapability
		}
		key := identifierField{strings.TrimRight(r, " "), strings.TrimRight(c, " ")}
		if !required[key] {
			return nil
		}
		if _, ok := widths[key]; ok {
			return ErrFingerprintCapability
		}
		w, e := checkedCatalogWidth(sql.NullInt64{Int64: n, Valid: true}, "fingerprint projection")
		if e != nil {
			return ErrFingerprintCapability
		}
		widths[key] = w
		return nil
	})
	addFingerprintStats(&f.Stats, stats)
	if err != nil {
		return nil, err
	}
	queries, err := fingerprintQueries(widths)
	if err != nil {
		return nil, err
	}
	manifest := fingerprintManifest()
	stats, err = reader(ctx, conn, queries, func(q int, columns []string, v []any) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if q < 0 || q >= len(manifest) {
			return ErrFingerprintCapability
		}
		p := manifest[q]
		expected := p.columns()
		if len(v) != len(expected) || len(columns) != len(expected) {
			return ErrFingerprintCapability
		}
		row := make(map[string]any, len(v))
		for i, c := range expected {
			if columns[i] != c {
				return fmt.Errorf("%w: projection labels for %s", ErrFingerprintCapability, p.Relation)
			}
			row[c] = v[i]
		}
		for _, c := range p.IdentifierColumns {
			if row[c] == nil {
				continue
			}
			s, ok := row[c].(string)
			if !ok {
				return fmt.Errorf("%w: identifier value for %s.%s", ErrFingerprintCapability, p.Relation, c)
			}
			row[c] = strings.TrimRight(s, " ")
		}
		var keys, values []any
		for _, c := range p.KeyColumns {
			keys = append(keys, row[c])
		}
		for _, c := range expected {
			values = append(values, row[c])
		}
		var key bytes.Buffer
		for _, v := range keys {
			if e := fingerprintToken(&key, v); e != nil {
				return e
			}
		}
		d, e := fingerprintDigest(values)
		if e != nil {
			return e
		}
		owners, links := fingerprintRouting(p.Relation, row)
		var systemFlag sql.NullInt64
		if value := row["RDB$SYSTEM_FLAG"]; value != nil {
			n, ok := value.(int64)
			if !ok {
				return fmt.Errorf("%w: SYSTEM_FLAG scalar", ErrFingerprintCapability)
			}
			systemFlag = sql.NullInt64{Int64: n, Valid: true}
		}
		r := FingerprintRecord{Family: p.Relation, Key: hex.EncodeToString(key.Bytes()), Digest: d, Count: 1, SystemFlag: systemFlag, Owners: owners, Links: links}
		return addFingerprintRecord(f, r, limit)
	})
	addFingerprintStats(&f.Stats, stats)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.Stats.BlobOpens != 0 || f.Stats.SourceBytes != 0 {
		return nil, ErrFingerprintCapability
	}
	// Empty families are represented by the fixed versioned manifest; their
	// counts are the sum of record multiplicities (zero without records).
	if f.AccountedBytes == 0 {
		cost := fingerprintAllocation(int64(unsafe.Sizeof(*f))) + fingerprintAllocation(int64(len(f.Version)))
		if cost > limit {
			return nil, ErrFingerprintLimit
		}
		f.Version = strings.Clone(f.Version)
		f.AccountedBytes = cost
	}
	return f, nil
}

func fingerprintRouting(family string, row map[string]any) (owners []CatalogObject, links []CatalogLink) {
	name := func(c string) string { s, _ := row["RDB$"+c].(string); return s }
	number := func(c string) sql.NullInt64 {
		n, ok := row["RDB$"+c].(int64)
		return sql.NullInt64{Int64: n, Valid: ok}
	}
	object := func(kind, c string) CatalogObject { return CatalogObject{Kind: kind, Name: name(c)} }
	own := func(o CatalogObject) { owners = append(owners, o) }
	link := func(a, b CatalogObject, field string) {
		if a.Name != "" && b.Name != "" {
			links = append(links, CatalogLink{a, b, field})
		}
	}
	security := func(o CatalogObject) {
		link(o, object("principal", "OWNER_NAME"), "OWNER_NAME")
		link(o, object("security class", "SECURITY_CLASS"), "SECURITY_CLASS")
		link(o, object("security class", "DEFAULT_CLASS"), "DEFAULT_CLASS")
	}
	relation := object("relation", "RELATION_NAME")
	domain := object("domain", "FIELD_SOURCE")
	switch family {
	case "RDB$RELATIONS":
		if row["RDB$VIEW_BLR"] != nil {
			relation.Kind = "view"
		}
		own(relation)
		security(relation)
	case "RDB$RELATION_FIELDS":
		own(relation)
		link(relation, domain, name("FIELD_NAME"))
		security(relation)
		if n := number("COLLATION_ID"); n.Valid {
			link(relation, CatalogObject{Kind: "column collation", Name: strconv.FormatInt(n.Int64, 10), Parent: domain.Name}, name("FIELD_NAME"))
		}
		if n := number("VIEW_CONTEXT"); n.Valid {
			link(CatalogObject{Kind: "view column", Name: name("FIELD_NAME"), Parent: relation.Name}, CatalogObject{Kind: "view context", Name: strconv.FormatInt(n.Int64, 10), Parent: relation.Name}, name("BASE_FIELD"))
		}
	case "RDB$VIEW_RELATIONS":
		view := object("view", "VIEW_NAME")
		own(view)
		n := number("VIEW_CONTEXT")
		link(CatalogObject{Kind: "view context", Name: strconv.FormatInt(n.Int64, 10), Parent: view.Name}, relation, name("CONTEXT_NAME"))
	case "RDB$FIELDS", "RDB$FIELD_DIMENSIONS":
		d := object("domain", "FIELD_NAME")
		own(d)
		if n := number("CHARACTER_SET_ID"); n.Valid {
			link(d, CatalogObject{Kind: "character set", Name: strconv.FormatInt(n.Int64, 10)}, "CHARACTER_SET_ID")
		}
		if n := number("COLLATION_ID"); n.Valid {
			cs := number("CHARACTER_SET_ID")
			link(d, CatalogObject{Kind: "collation", Name: strconv.FormatInt(n.Int64, 10), Parent: strconv.FormatInt(cs.Int64, 10)}, "COLLATION_ID")
		}
	case "RDB$PROCEDURES":
		p := object("procedure", "PROCEDURE_NAME")
		own(p)
		security(p)
	case "RDB$PROCEDURE_PARAMETERS":
		p := object("procedure", "PROCEDURE_NAME")
		own(p)
		link(p, domain, name("PARAMETER_NAME"))
	case "RDB$TRIGGERS", "RDB$TRIGGER_MESSAGES":
		tr := object("trigger", "TRIGGER_NAME")
		own(tr)
		if relation.Name != "" {
			own(relation)
			link(tr, relation, "")
		}
	case "RDB$INDICES", "RDB$INDEX_SEGMENTS":
		idx := object("index", "INDEX_NAME")
		own(idx)
		if relation.Name != "" {
			own(relation)
			link(idx, relation, "")
		}
		link(idx, object("index", "FOREIGN_KEY"), "FOREIGN_KEY")
		if name("FIELD_NAME") != "" {
			link(idx, CatalogObject{Kind: "index field", Name: name("FIELD_NAME"), Parent: idx.Name}, name("FIELD_NAME"))
		}
	case "RDB$RELATION_CONSTRAINTS":
		c := object("constraint", "CONSTRAINT_NAME")
		own(c)
		own(relation)
		link(c, relation, name("CONSTRAINT_TYPE"))
		link(c, object("index", "INDEX_NAME"), "INDEX_NAME")
	case "RDB$REF_CONSTRAINTS":
		c := object("constraint", "CONSTRAINT_NAME")
		own(c)
		link(c, object("constraint", "CONST_NAME_UQ"), "FOREIGN_KEY")
	case "RDB$CHECK_CONSTRAINTS":
		c := object("constraint", "CONSTRAINT_NAME")
		own(c)
		link(c, object("trigger", "TRIGGER_NAME"), "CHECK")
	case "RDB$GENERATORS":
		own(object("generator", "GENERATOR_NAME"))
	case "RDB$EXCEPTIONS":
		own(object("exception", "EXCEPTION_NAME"))
	case "RDB$FUNCTIONS", "RDB$FUNCTION_ARGUMENTS":
		own(object("function", "FUNCTION_NAME"))
	case "RDB$DEPENDENCIES":
		a := object("", "DEPENDENT_NAME")
		b := object("", "DEPENDED_ON_NAME")
		a.NativeType = number("DEPENDENT_TYPE")
		b.NativeType = number("DEPENDED_ON_TYPE")
		a.Kind = DependencyObjectType(a.NativeType.Int64).String()
		b.Kind = DependencyObjectType(b.NativeType.Int64).String()
		own(a)
		link(a, b, name("FIELD_NAME"))
	case "RDB$CHARACTER_SETS":
		n := number("CHARACTER_SET_ID")
		own(CatalogObject{Kind: "character set", Name: strconv.FormatInt(n.Int64, 10)})
	case "RDB$COLLATIONS":
		n := number("COLLATION_ID")
		cs := number("CHARACTER_SET_ID")
		own(CatalogObject{Kind: "collation", Name: strconv.FormatInt(n.Int64, 10), Parent: strconv.FormatInt(cs.Int64, 10)})
	case "RDB$USER_PRIVILEGES":
		a := object("grantee", "USER")
		a.NativeType = number("USER_TYPE")
		b := relation
		b.NativeType = number("OBJECT_TYPE")
		b.Kind = DependencyObjectType(b.NativeType.Int64).String()
		own(CatalogObject{Kind: "security"})
		link(a, b, name("FIELD_NAME"))
	case "RDB$ROLES", "RDB$SECURITY_CLASSES", "RDB$DATABASE":
		own(CatalogObject{Kind: "security"})
	}
	return
}
func fingerprintToken(h io.Writer, v any) error {
	var tag byte
	var b []byte
	var fixed [8]byte
	switch x := v.(type) {
	case nil:
		tag = 0
	case string:
		tag = 1
		b = []byte(x)
	case []byte:
		tag = 2
		b = x
	case int64:
		tag = 3
		binary.BigEndian.PutUint64(fixed[:], uint64(x))
		b = fixed[:]
	case bool:
		tag = 4
		if x {
			fixed[0] = 1
		}
		b = fixed[:1]
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Errorf("schema: nonfinite fingerprint number")
		}
		tag = 5
		binary.BigEndian.PutUint64(fixed[:], math.Float64bits(x))
		b = fixed[:]
	case CatalogBlobIdentity:
		tag = 6
		binary.BigEndian.PutUint32(fixed[:4], uint32(x.High))
		binary.BigEndian.PutUint32(fixed[4:], x.Low)
		b = fixed[:]
	default:
		return fmt.Errorf("schema: unsupported fingerprint value type %T", v)
	}
	h.Write([]byte{tag})
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(b)))
	h.Write(length[:])
	h.Write(b)
	return nil
}
