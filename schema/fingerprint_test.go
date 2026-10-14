package schema

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

func fingerprintTestKey(values ...any) string {
	var b bytes.Buffer
	for _, v := range values {
		if err := fingerprintToken(&b, v); err != nil {
			panic(err)
		}
	}
	return hex.EncodeToString(b.Bytes())
}

// Counts are the reviewed offline worst-distinct reference scale, not live
// catalog measurements. Domain/column/object strings are realistically shared.
func TestFingerprintReferenceCapacity(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f := &CatalogFingerprint{Version: fingerprintVersion}
	limit := int64(64 << 20)
	ordinary := 0
	add := func(family string, row map[string]any, key ...any) {
		t.Helper()
		owners, links := fingerprintRouting(family, row)
		digest, err := fingerprintDigest(key)
		if err != nil {
			t.Fatal(err)
		}
		r := FingerprintRecord{Family: family, Key: fingerprintTestKey(key...), Digest: digest, Count: 1, Owners: owners, Links: links}
		if err := addFingerprintRecord(f, r, limit); err != nil {
			t.Fatalf("capacity ordinary=%d accounted=%d family=%s: %v", ordinary, f.AccountedBytes, family, err)
		}
	}
	for i := 0; i < 80000; i++ {
		table := fmt.Sprintf("APP_TABLE_%04d", i/16)
		col := fmt.Sprintf("COLUMN_%02d", i%16)
		domain := fmt.Sprintf("RDB$FIELD_%05d", i%50000)
		add("RDB$RELATION_FIELDS", map[string]any{"RDB$RELATION_NAME": table, "RDB$FIELD_NAME": col, "RDB$FIELD_SOURCE": domain}, table, col)
		ordinary++
	}
	for i := 0; i < 50000; i++ {
		domain := fmt.Sprintf("RDB$FIELD_%05d", i)
		add("RDB$FIELDS", map[string]any{"RDB$FIELD_NAME": domain, "RDB$CHARACTER_SET_ID": int64(0), "RDB$COLLATION_ID": int64(0)}, domain)
		ordinary++
	}
	for i := 0; i < 6000; i++ {
		table := fmt.Sprintf("APP_TABLE_%04d", i)
		add("RDB$RELATIONS", map[string]any{"RDB$RELATION_NAME": table, "RDB$OWNER_NAME": "SYSDBA", "RDB$SECURITY_CLASS": "SQL$SHARED_CLASS"}, table)
		ordinary++
	}
	for i := 0; i < 3000; i++ {
		proc := fmt.Sprintf("APP_PROCEDURE_%04d", i)
		add("RDB$PROCEDURES", map[string]any{"RDB$PROCEDURE_NAME": proc, "RDB$OWNER_NAME": "SYSDBA"}, proc)
		ordinary++
	}
	for i := 0; i < 12000; i++ {
		proc := fmt.Sprintf("APP_PROCEDURE_%04d", i/4)
		param := fmt.Sprintf("PARAM_%d", i%4)
		domain := fmt.Sprintf("RDB$FIELD_%05d", i)
		add("RDB$PROCEDURE_PARAMETERS", map[string]any{"RDB$PROCEDURE_NAME": proc, "RDB$PARAMETER_NAME": param, "RDB$FIELD_SOURCE": domain}, proc, int64(0), int64(i%4), param)
		ordinary++
	}
	for i := 0; i < 227; i++ {
		name := fmt.Sprintf("APP_GENERATOR_%03d", i)
		add("RDB$GENERATORS", map[string]any{"RDB$GENERATOR_NAME": name}, name)
		ordinary++
	}
	for i := 0; i < 142527; i++ {
		dependent := fmt.Sprintf("APP_PROCEDURE_%04d", i/1000)
		table := fmt.Sprintf("APP_TABLE_%04d", i%6000)
		var field any
		if i%100 < 82 {
			field = fmt.Sprintf("COLUMN_%02d", i%16)
		}
		add("RDB$DEPENDENCIES", map[string]any{"RDB$DEPENDENT_NAME": dependent, "RDB$DEPENDENT_TYPE": int64(5), "RDB$DEPENDED_ON_NAME": table, "RDB$DEPENDED_ON_TYPE": int64(0), "RDB$FIELD_NAME": field}, dependent, int64(5), table, int64(0), field)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(f)
	t.Logf("ordinary=%d dependency=142527 records=%d accounted=%d retained-heap-delta=%d", ordinary, f.RecordCount(), f.AccountedBytes, int64(after.HeapAlloc)-int64(before.HeapAlloc))
	if f.RecordCount() != 293754 || ordinary != 151227 || f.AccountedBytes > limit {
		t.Fatal("capacity/count mismatch")
	}
	// Actual retained allocation sanity, independent of the admission formula.
	// GC heap deltas are observational; 1 MiB slack accommodates test/runtime
	// noise without using RSS or pretending this is a live performance SLA.
	if delta := int64(after.HeapAlloc) - int64(before.HeapAlloc); delta > f.AccountedBytes+(1<<20) {
		t.Fatalf("retained heap %d exceeds accounted %d", delta, f.AccountedBytes)
	}
	t.Logf("layout entry=%d object=%d link=%d names=%d objects=%d keys=%d ownerrefs=%d links=%d", unsafe.Sizeof(fingerprintEntry{}), unsafe.Sizeof(fingerprintObject{}), unsafe.Sizeof(fingerprintLink{}), f.storage.names.n, f.storage.objects.n, f.storage.keys.n, f.storage.owners.n, f.storage.links.n)
	// Exercise a real 64 MiB capacity boundary using more distinct catalog
	// tuples, not an artificially cheap accounting constant or larger cap.
	for i := 142527; ; i++ {
		dependent := fmt.Sprintf("APP_PROCEDURE_%04d", i/1000)
		table := fmt.Sprintf("APP_TABLE_%04d", i%6000)
		field := fmt.Sprintf("COLUMN_%02d", i%16)
		row := map[string]any{"RDB$DEPENDENT_NAME": dependent, "RDB$DEPENDENT_TYPE": int64(5), "RDB$DEPENDED_ON_NAME": table, "RDB$DEPENDED_ON_TYPE": int64(0), "RDB$FIELD_NAME": field}
		owners, links := fingerprintRouting("RDB$DEPENDENCIES", row)
		r := FingerprintRecord{Family: "RDB$DEPENDENCIES", Key: fingerprintTestKey(dependent, int64(5), table, int64(0), field), Count: 1, Owners: owners, Links: links}
		before := fingerprintStorageSnapshot(f)
		err := addFingerprintRecord(f, r, limit)
		if errors.Is(err, ErrFingerprintLimit) {
			if before != fingerprintStorageSnapshot(f) {
				t.Fatal("budget rejection grew retained capacity")
			}
			if _, ok := f.LookupRecord(r.Family, r.Key); ok {
				t.Fatal("rejected tuple retained")
			}
			t.Logf("64MiB rejection records=%d accounted=%d additional-dependencies=%d", f.RecordCount(), f.AccountedBytes, i-142527)
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func fingerprintStorageSnapshot(f *CatalogFingerprint) [25]int64 {
	s := &f.storage
	return [25]int64{f.AccountedBytes, int64(s.entries.n), int64(s.names.n), int64(s.objects.n), int64(s.links.n), int64(s.owners.n), int64(s.text.n), int64(s.keys.n), int64(len(s.entries.chunks)), int64(cap(s.entries.chunks)), int64(len(s.names.chunks)), int64(cap(s.names.chunks)), int64(len(s.objects.chunks)), int64(cap(s.objects.chunks)), int64(len(s.links.chunks)), int64(cap(s.links.chunks)), int64(len(s.owners.chunks)), int64(cap(s.owners.chunks)), int64(len(s.text.chunks)), int64(cap(s.text.chunks)), int64(len(s.keys.chunks)), int64(cap(s.keys.chunks)), int64(len(s.nameIndex)), int64(len(s.objectIndex)), int64(len(s.recordIndex))}
}

func TestFingerprintCompactExactKeyAndOwnedRouting(t *testing.T) {
	key := fingerprintTestKey(nil, "", []byte{}, int64(math.MinInt64), int64(math.MaxInt64), false, true, float64(-1.25), math.Copysign(0, -1), CatalogBlobIdentity{High: -7, Low: math.MaxUint32}, "Quotedé\xff"+strings.Repeat("Long", 2500), []byte{0, 255, 4})
	r := FingerprintRecord{Family: "RDB$DEPENDENCIES", Key: key, Count: 1, Owners: []CatalogObject{{Kind: "type 99", Name: "é\xff", Parent: "Exact", NativeType: sql.NullInt64{Int64: 99, Valid: true}}}, Links: []CatalogLink{{From: CatalogObject{Kind: "view", Name: "Exact"}, To: CatalogObject{Kind: "relation", Name: "exact", NativeType: sql.NullInt64{Int64: 42, Valid: false}}, Field: "Column"}}}
	f := &CatalogFingerprint{Version: "v"}
	if err := addFingerprintRecord(f, r, 1<<20); err != nil {
		t.Fatal(err)
	}
	saved, ok := f.LookupRecord(r.Family, key)
	if !ok || !reflect.DeepEqual(saved, r) {
		t.Fatalf("exact round trip failed ok=%v", ok)
	}
	saved.Owners[0].Name = "corrupted"
	saved.Links[0].To.Name = "corrupted"
	again, ok := f.LookupRecord(r.Family, key)
	if !ok || !reflect.DeepEqual(again, r) {
		t.Fatal("DTO mutation changed storage")
	}
	second:=r;second.Key=fingerprintTestKey("second");if err:=addFingerprintRecord(f,second,1<<20);err!=nil{t.Fatal(err)}
	n := 0
	f.VisitRecords(func(got FingerprintRecord) bool {
		n++
		if !reflect.DeepEqual(got, r) {
			t.Fatal("iteration changed DTO")
		}
		return false
	})
	if n != 1 {
		t.Fatal(n)
	}
	for _, bad := range []string{"nothex", "00", "000000000000000001", "ff0000000000000000"} {
		before := fingerprintStorageSnapshot(f)
		r.Key = bad
		if err := addFingerprintRecord(f, r, 1<<20); err == nil {
			t.Fatal("invalid identity accepted")
		}
		if before != fingerprintStorageSnapshot(f) {
			t.Fatal("invalid identity retained data")
		}
		if _, ok := f.LookupRecord(r.Family, bad); ok {
			t.Fatal("invalid lookup succeeded")
		}
	}
}

func TestFingerprintCompactGrantMultisetAndTypedIdentities(t *testing.T) {
 f:=&CatalogFingerprint{Version:"v"}
 fields:=[]any{nil,"",int64(0),false,[]byte{},CatalogBlobIdentity{}}
 for _,field:=range fields {key:=fingerprintTestKey("User","Grantor","S",int64(1),"Table",field,int64(8),int64(0));digest,err:=fingerprintDigest([]any{field});if err!=nil{t.Fatal(err)};r:=FingerprintRecord{Family:"RDB$USER_PRIVILEGES",Key:key,Digest:digest,Count:1,Owners:[]CatalogObject{{Kind:"security"}}};if err:=addFingerprintRecord(f,r,1<<20);err!=nil{t.Fatal(err)};if err:=addFingerprintRecord(f,r,1<<20);err!=nil{t.Fatal(err)};got,ok:=f.LookupRecord(r.Family,key);if !ok||got.Key!=key||got.Count!=2||got.Digest!=digest {t.Fatal("tuple/multiplicity lost")}}
 if f.RecordCount()!=len(fields){t.Fatal("typed identities collapsed")}
 var nilFingerprint *CatalogFingerprint;if nilFingerprint.RecordCount()!=0||!nilFingerprint.Equal(nil){t.Fatal("nil model behavior")};if _,ok:=f.LookupRecord("missing",fingerprintTestKey("Table"));ok {t.Fatal("unknown family found")}
}

func TestFingerprintIndexesResolveCollisions(t *testing.T) {
	index := make([]uint32, 256)
	for id := uint32(1); id <= 100; id++ {
		fingerprintIndexInsert(index, 7, id)
	}
	for id := uint32(1); id <= 100; id++ {
		got, ok := fingerprintIndexFind(index, 7, func(v uint32) bool { return v == id })
		if !ok || got != id {
			t.Fatal("collision lost identity", id)
		}
	}
	if _, ok := fingerprintIndexFind(index, 7, func(uint32) bool { return false }); ok {
		t.Fatal("hash-only match")
	}
}

func TestFingerprintGrowthAdmissionIncludesOldBacking(t *testing.T) {
	f := &CatalogFingerprint{Version: "v"}
	for i := 0; i < 192; i++ {
		r := FingerprintRecord{Family: "RDB$GENERATORS", Key: fingerprintTestKey(int64(i)), Count: 1}
		if err := addFingerprintRecord(f, r, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	before := fingerprintStorageSnapshot(f)
	r := FingerprintRecord{Family: "RDB$GENERATORS", Key: fingerprintTestKey(int64(192)), Count: 1}
	// 193 entries grow the index beyond 75% occupancy. Final table rounding is
	// unchanged here, but the still-live old backing must be admitted as well.
	if err := addFingerprintRecord(f, r, f.AccountedBytes); !errors.Is(err, ErrFingerprintLimit) {
		t.Fatal("old index backing not charged", err)
	}
	if before != fingerprintStorageSnapshot(f) {
		t.Fatal("failed growth mutated storage")
	}
	if err := addFingerprintRecord(f, r, f.AccountedBytes+8192); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintNullEmptyAndIdentityDiffer(t *testing.T) {
	values := []any{nil, "", []byte{}, int64(0), false, float64(0), CatalogBlobIdentity{High: 0, Low: 0}}
	seen := map[[32]byte]bool{}
	for _, v := range values {
		d, err := fingerprintDigest([]any{v})
		if err != nil {
			t.Fatal(err)
		}
		if seen[d] {
			t.Fatalf("collision for %T", v)
		}
		seen[d] = true
	}
	for _, v := range []any{1, math.NaN(), math.Inf(1)} {
		if _, err := fingerprintDigest([]any{v}); err == nil {
			t.Fatalf("accepted %T", v)
		}
	}
	a, _ := fingerprintDigest([]any{"a", "bc"})
	b, _ := fingerprintDigest([]any{"ab", "c"})
	if a == b {
		t.Fatal("ambiguous concatenation")
	}
}

func TestFingerprintDefinitionCoverage(t *testing.T) {
	// Production loader/detail projections are the independent coverage oracle;
	// file/runtime administrative configuration and statistics are excluded.
	aliases := map[string]string{"r": "RELATIONS", "rf": "RELATION_FIELDS", "f": "FIELDS", "cs": "CHARACTER_SETS", "co": "COLLATIONS", "rco": "COLLATIONS", "v": "VIEW_RELATIONS", "p": "PROCEDURES", "pp": "PROCEDURE_PARAMETERS", "g": "GENERATORS", "e": "EXCEPTIONS", "i": "INDICES", "s": "INDEX_SEGMENTS", "c": "RELATION_CONSTRAINTS", "cr": "RELATIONS", "pc": "RELATION_CONSTRAINTS", "rc": "RELATION_CONSTRAINTS", "k": "CHECK_CONSTRAINTS", "t": "TRIGGERS", "a": "FUNCTION_ARGUMENTS", "d": "DEPENDENCIES"}
	have := map[string]bool{}
	for _, p := range fingerprintManifest() {
		for _, c := range p.columns() {
			have[p.Relation+"."+c] = true
		}
	}
	re := regexp.MustCompile(`([a-z]+)\.(RDB\$[A-Z_]+)`)
	for _, file := range []string{"schema.go", "catalog_extended.go", "table_description.go"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			if m[2] == "RDB$STATISTICS" {
				continue
			}
			relation := aliases[m[1]]
			if relation == "" {
				continue
			}
			key := "RDB$" + relation + "." + m[2]
			if !have[key] {
				// Reused SQL aliases in extended projections have distinct families.
				if file == "catalog_extended.go" && (m[1] == "f" || m[1] == "r" || m[1] == "p") {
					found := false
					for _, fam := range []string{"FUNCTIONS", "FILES", "REF_CONSTRAINTS", "ROLES", "USER_PRIVILEGES"} {
						if have["RDB$"+fam+"."+m[2]] {
							found = true
						}
					}
					if found || strings.HasPrefix(m[2], "RDB$FILE_") || m[2] == "RDB$SHADOW_NUMBER" {
						continue
					}
				}
				t.Errorf("uncovered consumed definition %s (%s)", key, file)
			}
		}
	}
	for _, key := range []string{"RDB$RELATIONS.RDB$EXTERNAL_DESCRIPTION", "RDB$GENERATORS.RDB$DESCRIPTION", "RDB$EXCEPTIONS.RDB$DESCRIPTION", "RDB$EXCEPTIONS.RDB$SYSTEM_FLAG", "RDB$ROLES.RDB$DESCRIPTION"} {
		if !have[key] {
			t.Error("missing definition", key)
		}
	}
}

func TestFingerprintEveryManifestValueChanges(t *testing.T) {
	for _, p := range fingerprintManifest() {
		t.Run(p.Relation, func(t *testing.T) {
			cols := p.columns()
			base := fingerprintRow(p.Relation, map[string]any{})
			rows := map[string][][]any{p.Relation: {base}}
			a, err := ReadCatalogFingerprint(context.Background(), nil, 1<<20, fingerprintFixtureReader(rows, false, ""))
			if err != nil {
				t.Fatal(err)
			}
			for i, c := range cols {
				changed := append([]any(nil), base...)
				changed[i] = "different"
				if c == "RDB$SYSTEM_FLAG" {
					changed[i] = int64(1) // The retained routing scalar is typed.
				}
				rows[p.Relation] = [][]any{changed}
				b, err := ReadCatalogFingerprint(context.Background(), nil, 1<<20, fingerprintFixtureReader(rows, false, ""))
				if err != nil {
					t.Fatal(c, err)
				}
				if a.Equal(b) {
					t.Fatal("unhashed column", c)
				}
			}
		})
	}
}

func TestFingerprintOnlyTuplesAreMultisets(t *testing.T) {
	f := &CatalogFingerprint{Version: "v"}
	r := FingerprintRecord{Family: "RDB$RELATIONS", Key: fingerprintTestKey("Exact"), Count: 1}
	if err := addFingerprintRecord(f, r, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := addFingerprintRecord(f, r, 1<<20); err == nil {
		t.Fatal("duplicate keyed row accepted")
	}
}

func TestFingerprintSecurityAndColumnCollationRouting(t *testing.T) {
	_, links := fingerprintRouting("RDB$RELATIONS", map[string]any{"RDB$RELATION_NAME": "Table", "RDB$OWNER_NAME": "Owner", "RDB$SECURITY_CLASS": "Class", "RDB$DEFAULT_CLASS": "Default"})
	if len(links) != 3 {
		t.Fatalf("security facts missing: %+v", links)
	}
	_, links = fingerprintRouting("RDB$RELATION_FIELDS", map[string]any{"RDB$RELATION_NAME": "View", "RDB$FIELD_NAME": "Column", "RDB$FIELD_SOURCE": "Domain", "RDB$COLLATION_ID": int64(2), "RDB$VIEW_CONTEXT": int64(1), "RDB$BASE_FIELD": "Original"})
	found := false
	for _, l := range links {
		if l.To.Kind == "column collation" && l.To.Name == "2" && l.To.Parent == "Domain" && l.Field == "Column" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing override collation routing %+v", links)
	}
}

func TestFingerprintAdmissionOwnsAndInternsNames(t *testing.T) {
	f := &CatalogFingerprint{Version: "v"}
	r := FingerprintRecord{Family: "RDB$DEPENDENCIES", Key: fingerprintTestKey("a"), Count: 1, Owners: []CatalogObject{{Kind: "view", Name: "Exact"}}, Links: []CatalogLink{{From: CatalogObject{Kind: "view", Name: "Exact"}, To: CatalogObject{Kind: "relation", Name: "base"}}}}
	if err := addFingerprintRecord(f, r, 1<<20); err != nil {
		t.Fatal(err)
	}
	saved, ok := f.LookupRecord(r.Family, r.Key)
	if !ok {
		t.Fatal("record missing")
	}
	r.Owners[0].Name = "modified"
	r.Links[0].To.Name = "modified"
	if saved.Owners[0].Name != "Exact" || saved.Links[0].To.Name != "base" {
		t.Fatal("retained caller slices")
	}
	n := f.storage.names.n
	cost := f.AccountedBytes
	r.Key = fingerprintTestKey(strings.Repeat("b", 500000))
	r.Owners = saved.Owners
	r.Links = saved.Links
	if err := addFingerprintRecord(f, r, cost); !errors.Is(err, ErrFingerprintLimit) {
		t.Fatal(err)
	}
	if f.storage.names.n != n || f.AccountedBytes != cost || f.RecordCount() != 1 {
		t.Fatal("failed admission retained data")
	}
	r.Key = fingerprintTestKey("b")
	if err := addFingerprintRecord(f, r, 1<<20); err != nil {
		t.Fatal(err)
	}
	if f.storage.names.n != n+1 {
		t.Fatal("names not interned once", n, f.storage.names.n)
	}
}

func TestFingerprintEmptyStillAccountsHeader(t *testing.T) {
	reader := fingerprintFixtureReader(nil, false, "")
	f, err := ReadCatalogFingerprint(context.Background(), nil, 1<<20, reader)
	if err != nil {
		t.Fatal(err)
	}
	if f.AccountedBytes <= 0 {
		t.Fatal("empty fingerprint header not accounted")
	}
	if _, err := ReadCatalogFingerprint(context.Background(), nil, f.AccountedBytes-1, reader); !errors.Is(err, ErrFingerprintLimit) {
		t.Fatal(err)
	}
}

func TestFingerprintManifestIncludesSupportCatalogs(t *testing.T) {
	have := map[string]bool{}
	for _, f := range fingerprintFamilies() {
		have[f] = true
	}
	for _, f := range []string{"RDB$VIEW_RELATIONS", "RDB$FIELD_DIMENSIONS", "RDB$DEPENDENCIES", "RDB$USER_PRIVILEGES", "RDB$ROLES", "RDB$SECURITY_CLASSES"} {
		if !have[f] {
			t.Fatal("missing family", f)
		}
	}
	widths := map[identifierField]int{}
	for _, p := range fingerprintManifest() {
		for _, c := range p.columns() {
			widths[identifierField{p.Relation, c}] = 128
		}
	}
	qs, err := fingerprintQueries(widths)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range qs {
		for _, bad := range []string{"SELECT *", "GEN_ID(", "STATISTICS"} {
			if strings.Contains(q.SQL, bad) {
				t.Fatal(q.SQL)
			}
		}
	}
	delete(widths, identifierField{"RDB$RELATION_FIELDS", "RDB$BASE_FIELD"})
	if _, err := fingerprintQueries(widths); !errors.Is(err, ErrFingerprintCapability) {
		t.Fatal(err)
	}
}

func TestFingerprintBudgetAndMultiplicity(t *testing.T) {
	r := FingerprintRecord{Family: "RDB$DEPENDENCIES", Key: fingerprintTestKey("exact"), Count: 1, Owners: []CatalogObject{{Kind: "view", Name: strings.Repeat("Q", 128)}}, Links: []CatalogLink{{From: CatalogObject{Kind: "view", Name: "Exact"}, To: CatalogObject{Kind: "relation", Name: "exact"}, Field: "F"}}}
	f := &CatalogFingerprint{Version: "v"}
	if err := addFingerprintRecord(f, r, 1<<20); err != nil {
		t.Fatal(err)
	}
	n := f.AccountedBytes
	fail := &CatalogFingerprint{Version: "v"}
	if err := addFingerprintRecord(fail, r, n-1); !errors.Is(err, ErrFingerprintLimit) {
		t.Fatal(err)
	}
	if fail.RecordCount() != 0 || fail.AccountedBytes != 0 {
		t.Fatal("failed admission mutated state")
	}
	g := &CatalogFingerprint{Version: "v"}
	if err := addFingerprintRecord(g, r, n); err != nil {
		t.Fatal(err)
	}
	if !f.Equal(g) {
		t.Fatal("boundary mismatch")
	}
	g.AccountedBytes++
	g.Stats.Rows++
	if !f.Equal(g) {
		t.Fatal("metrics influence equality")
	}
	if err := addFingerprintRecord(g, r, n+1); err != nil {
		t.Fatal(err)
	}
	if f.Equal(g) {
		t.Fatal("duplicate ignored")
	}
	r.Count = math.MaxUint32
	if err := addFingerprintRecord(g, r, 1<<20); err == nil {
		t.Fatal("overflow accepted")
	}
	r.Count = 1
	r.Digest[0] = 1
	if err := addFingerprintRecord(g, r, 1<<20); err == nil {
		t.Fatal("conflicting keyed row accepted")
	}
}

// The injected reader supplies discovery plus real row visitor inputs without
// importing the native package or requiring database/sql fake-driver machinery.
func fingerprintFixtureReader(rows map[string][][]any, reverse bool, missing string) CatalogFingerprintReader {
	return func(ctx context.Context, c *sql.Conn, qs []CatalogFingerprintQuery, visit CatalogFingerprintVisitor) (CatalogFingerprintStats, error) {
		var s CatalogFingerprintStats
		for q, query := range qs {
			s.Queries++
			if len(query.Args) == 2 {
				s.Rows++
				if err := visit(q, []string{"RDB$FIELD_LENGTH"}, []any{int64(128)}); err != nil {
					return s, err
				}
				continue
			}
			if strings.Contains(query.SQL, "JOIN RDB$FIELDS") {
				for _, p := range fingerprintManifest() {
					for _, col := range p.columns() {
						if p.Relation+"."+col == missing {
							continue
						}
						s.Rows++
						if err := visit(q, []string{"RDB$RELATION_NAME", "RDB$FIELD_NAME", "RDB$FIELD_LENGTH"}, []any{p.Relation, col, int64(128)}); err != nil {
							return s, err
						}
					}
				}
				continue
			}
			p := fingerprintManifest()[q]
			rs := rows[p.Relation]
			for i := range rs {
				j := i
				if reverse {
					j = len(rs) - 1 - i
				}
				s.Rows++
				if err := visit(q, p.columns(), rs[j]); err != nil {
					return s, err
				}
			}
		}
		return s, nil
	}
}
func fingerprintRow(family string, values map[string]any) []any {
	for _, p := range fingerprintManifest() {
		if p.Relation == family {
			var row []any
			for _, c := range p.columns() {
				row = append(row, values[c])
			}
			return row
		}
	}
	panic("bad test family")
}
func TestFingerprintReaderReorderCaseAndRouting(t *testing.T) {
	row := func(name string) []any {
		return fingerprintRow("RDB$RELATION_FIELDS", map[string]any{"RDB$RELATION_NAME": name + "  ", "RDB$FIELD_NAME": "quoted Column ", "RDB$FIELD_SOURCE": "Domain"})
	}
	dep := fingerprintRow("RDB$DEPENDENCIES", map[string]any{"RDB$DEPENDENT_NAME": "View", "RDB$DEPENDENT_TYPE": int64(1), "RDB$DEPENDED_ON_NAME": "Table", "RDB$DEPENDED_ON_TYPE": int64(0)})
	rows := map[string][][]any{"RDB$RELATION_FIELDS": {row(strings.Repeat("a", 128)), row("Exact"), row("exact")}, "RDB$DEPENDENCIES": {dep, dep}}
	a, err := ReadCatalogFingerprint(context.Background(), nil, 1<<20, fingerprintFixtureReader(rows, false, ""))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ReadCatalogFingerprint(context.Background(), nil, 1<<20, fingerprintFixtureReader(rows, true, ""))
	if err != nil || !a.Equal(b) {
		t.Fatal("reorder", err)
	}
	count := 0
	a.VisitRecords(func(r FingerprintRecord) bool {
		if r.Family == "RDB$RELATION_FIELDS" {
			count++
			if r.Owners[0].Name == "" || len(r.Links) != 1 || r.Links[0].To.Name != "Domain" {
				t.Fatal(r)
			}
		}
		if r.Family == "RDB$DEPENDENCIES" && r.Count != 2 {
			t.Fatal(r)
		}
		return true
	})
	if count != 3 {
		t.Fatal(count)
	}
	_, err = ReadCatalogFingerprint(context.Background(), nil, 1<<20, fingerprintFixtureReader(rows, false, "RDB$FIELDS.RDB$COMPUTED_BLR"))
	if !errors.Is(err, ErrFingerprintCapability) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadCatalogFingerprint(ctx, nil, 1<<20, fingerprintFixtureReader(rows, false, "")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
