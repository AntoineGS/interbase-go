package schema

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"unsafe"
)

// Retained storage has no per-row Go maps, strings or routing slice headers.
// Exact typed keys use interned string IDs and reversible scalar encodings;
// digests are never substituted for identity. DTOs are reconstructed one at a
// time. All pools and hash tables have explicit, admitted backing capacities.
type fingerprintEntry struct {
	family, keyStart, keyLen, count          uint32
	digest                                   [32]byte
	ownerStart, ownerLen, linkStart, linkLen uint32
	systemFlag                               int64
	systemFlagValid                          bool
}
type fingerprintObject struct {
	kind, name, parent, valid uint32
	native                    int64
}
type fingerprintLink struct{ from, to, field uint32 }
type fingerprintName struct{ start, length uint32 }
type fingerprintPool[T any] struct {
	chunks [][]T
	n      uint32
}
type fingerprintStorage struct {
	entries                             fingerprintPool[fingerprintEntry]
	names                               fingerprintPool[fingerprintName]
	objects                             fingerprintPool[fingerprintObject]
	links                               fingerprintPool[fingerprintLink]
	owners                              fingerprintPool[uint32]
	text, keys                          fingerprintPool[byte]
	nameIndex, objectIndex, recordIndex []uint32
}

// 8KiB rounding conservatively covers allocator classes/page rounding. Chunks
// are page-sized multiples; directories and tables include their unused slots.
func fingerprintAllocation(n int64) int64 {
	if n == 0 {
		return 0
	}
	return (n + 8191) / 8192 * 8192
}
func fingerprintChunkSize[T any]() uint32 {
	var v T
	switch unsafe.Sizeof(v) {
	case 1:
		return 8192
	case 4:
		return 2048
	default:
		return 1024
	}
}
func fingerprintDirectoryCapacity(current, needed int) int {
	if needed <= current {
		return current
	}
	n := current
	if n == 0 {
		n = 1
	}
	for n < needed {
		n *= 2
	}
	return n
}
func (p *fingerprintPool[T]) cost(n uint32) (retained, peakExtra int64) {
	size := fingerprintChunkSize[T]()
	chunks := (uint64(n) + uint64(size) - 1) / uint64(size)
	dir := fingerprintDirectoryCapacity(cap(p.chunks), int(chunks))
	var v T
	retained = int64(chunks)*fingerprintAllocation(int64(size)*int64(unsafe.Sizeof(v))) + fingerprintAllocation(int64(dir)*int64(unsafe.Sizeof([]T{})))
	if dir > cap(p.chunks) {
		peakExtra = fingerprintAllocation(int64(cap(p.chunks)) * int64(unsafe.Sizeof([]T{})))
	}
	return
}
func (p *fingerprintPool[T]) ensure(n uint32) {
	size := fingerprintChunkSize[T]()
	needed := int((uint64(n) + uint64(size) - 1) / uint64(size))
	capacity := fingerprintDirectoryCapacity(cap(p.chunks), needed)
	if capacity > cap(p.chunks) {
		d := make([][]T, len(p.chunks), capacity)
		copy(d, p.chunks)
		p.chunks = d
	}
	for len(p.chunks) < needed {
		p.chunks = append(p.chunks, make([]T, size))
	}
}
func (p *fingerprintPool[T]) at(i uint32) T {
	size := fingerprintChunkSize[T]()
	return p.chunks[i/size][i%size]
}
func (p *fingerprintPool[T]) set(i uint32, v T) {
	size := fingerprintChunkSize[T]()
	p.chunks[i/size][i%size] = v
}
func (p *fingerprintPool[T]) append(v T) { p.set(p.n, v); p.n++ }

func fingerprintHashBytes(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, v := range b {
		h = (h ^ uint64(v)) * 1099511628211
	}
	return h
}
func fingerprintHashString(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint64(s[i])) * 1099511628211
	}
	return h
}
func fingerprintObjectHash(o fingerprintObject) uint64 {
	var b [24]byte
	binary.LittleEndian.PutUint32(b[:4], o.kind)
	binary.LittleEndian.PutUint32(b[4:8], o.name)
	binary.LittleEndian.PutUint32(b[8:12], o.parent)
	binary.LittleEndian.PutUint32(b[12:16], o.valid)
	binary.LittleEndian.PutUint64(b[16:], uint64(o.native))
	return fingerprintHashBytes(b[:])
}
func fingerprintIndexFind(index []uint32, h uint64, equal func(uint32) bool) (uint32, bool) {
	if len(index) == 0 {
		return 0, false
	}
	mask := uint64(len(index) - 1)
	for pos := h & mask; ; pos = (pos + 1) & mask {
		id := index[pos]
		if id == 0 {
			return 0, false
		}
		if equal(id) {
			return id, true
		}
	}
}
func fingerprintIndexInsert(index []uint32, h uint64, id uint32) {
	mask := uint64(len(index) - 1)
	for pos := h & mask; ; pos = (pos + 1) & mask {
		if index[pos] == 0 {
			index[pos] = id
			return
		}
	}
}
func fingerprintIndexCapacity(current int, n uint32) int {
	capacity := current
	if capacity == 0 && n != 0 {
		capacity = 256
	}
	for uint64(n)*4 > uint64(capacity)*3 {
		capacity *= 2
	}
	return capacity
}
func fingerprintIndexCost(index []uint32, n uint32) (retained, extra int64) {
	capacity := fingerprintIndexCapacity(len(index), n)
	retained = fingerprintAllocation(int64(capacity) * 4)
	if capacity > len(index) {
		extra = fingerprintAllocation(int64(len(index)) * 4)
	}
	return
}
func fingerprintIndexEnsure(index *[]uint32, n uint32, hash func(uint32) uint64) {
	capacity := fingerprintIndexCapacity(len(*index), n)
	if capacity == len(*index) {
		return
	}
	next := make([]uint32, capacity)
	for _, id := range *index {
		if id != 0 {
			fingerprintIndexInsert(next, hash(id), id)
		}
	}
	*index = next
}
func fingerprintStoredEqual(pool *fingerprintPool[byte], start, length uint32, s string) bool {
	if uint64(length) != uint64(len(s)) {
		return false
	}
	for i := uint32(0); i < length; i++ {
		if pool.at(start+i) != s[i] {
			return false
		}
	}
	return true
}
func fingerprintStoredHash(pool *fingerprintPool[byte], start, length uint32) uint64 {
	h := uint64(14695981039346656037)
	for i := uint32(0); i < length; i++ {
		h = (h ^ uint64(pool.at(start+i))) * 1099511628211
	}
	return h
}
func fingerprintStoredBytes(pool *fingerprintPool[byte], start, length uint32) []byte {
	b := make([]byte, int(length))
	for i := range b {
		b[i] = pool.at(start + uint32(i))
	}
	return b
}
func (s *fingerprintStorage) name(id uint32) string {
	if id == 0 {
		return ""
	}
	n := s.names.at(id - 1)
	return string(fingerprintStoredBytes(&s.text, n.start, n.length))
}
func (s *fingerprintStorage) findName(name string) (uint32, bool) {
	if name == "" {
		return 0, true
	}
	return fingerprintIndexFind(s.nameIndex, fingerprintHashString(name), func(id uint32) bool {
		n := s.names.at(id - 1)
		return fingerprintStoredEqual(&s.text, n.start, n.length, name)
	})
}
func (s *fingerprintStorage) nameHash(id uint32) uint64 {
	n := s.names.at(id - 1)
	return fingerprintStoredHash(&s.text, n.start, n.length)
}
func (s *fingerprintStorage) recordHash(e fingerprintEntry) uint64 {
	return fingerprintStoredHash(&s.keys, e.keyStart, e.keyLen) ^ uint64(e.family)*0x9e3779b97f4a7c15
}
func (s *fingerprintStorage) findRecord(family uint32, key []byte) (uint32, bool) {
	return fingerprintIndexFind(s.recordIndex, fingerprintHashBytes(key)^uint64(family)*0x9e3779b97f4a7c15, func(id uint32) bool {
		e := s.entries.at(id - 1)
		if e.family != family || int(e.keyLen) != len(key) {
			return false
		}
		for i, v := range key {
			if s.keys.at(e.keyStart+uint32(i)) != v {
				return false
			}
		}
		return true
	})
}

// compactFingerprintKey parses the existing complete hex tuple, replacing only
// string payloads with lossless intern references. No formatted/hash identity.
func compactFingerprintKey(key string, name func(string) (uint32, bool)) ([]byte, error) {
	raw, err := hex.DecodeString(key)
	if err != nil {
		return nil, errors.New("schema: malformed fingerprint identity")
	}
	var out []byte
	for len(raw) > 0 {
		if len(raw) < 9 {
			return nil, errors.New("schema: truncated fingerprint identity")
		}
		tag := raw[0]
		length := binary.BigEndian.Uint64(raw[1:9])
		if length > uint64(len(raw)-9) {
			return nil, errors.New("schema: truncated fingerprint identity")
		}
		payload := raw[9 : 9+int(length)]
		raw = raw[9+int(length):]
		out = append(out, tag)
		switch tag {
		case 0:
			if length != 0 {
				return nil, errors.New("schema: invalid NULL identity")
			}
		case 1:
			id, ok := name(string(payload))
			if !ok {
				return nil, errors.New("schema: fingerprint identity not found")
			}
			out = binary.AppendUvarint(out, uint64(id))
		case 2:
			out = binary.AppendUvarint(out, length)
			out = append(out, payload...)
		case 3:
			if length != 8 {
				return nil, errors.New("schema: invalid integer identity")
			}
			out = binary.AppendVarint(out, int64(binary.BigEndian.Uint64(payload)))
		case 4:
			if length != 1 || payload[0] > 1 {
				return nil, errors.New("schema: invalid boolean identity")
			}
			out = append(out, payload...)
		case 5, 6:
			if length != 8 {
				return nil, errors.New("schema: invalid scalar identity")
			}
			out = append(out, payload...)
		default:
			return nil, errors.New("schema: unsupported identity token")
		}
	}
	return out, nil
}
func (s *fingerprintStorage) exactKey(e fingerprintEntry) string {
	key := fingerprintStoredBytes(&s.keys, e.keyStart, e.keyLen)
	var raw bytes.Buffer
	for len(key) > 0 {
		tag := key[0]
		key = key[1:]
		var payload []byte
		var fixed [8]byte
		switch tag {
		case 0:
		case 1:
			id, n := binary.Uvarint(key)
			key = key[n:]
			payload = []byte(s.name(uint32(id)))
		case 2:
			length, n := binary.Uvarint(key)
			key = key[n:]
			payload = key[:int(length)]
			key = key[int(length):]
		case 3:
			v, n := binary.Varint(key)
			key = key[n:]
			binary.BigEndian.PutUint64(fixed[:], uint64(v))
			payload = fixed[:]
		case 4:
			payload = key[:1]
			key = key[1:]
		case 5, 6:
			payload = key[:8]
			key = key[8:]
		}
		raw.WriteByte(tag)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(payload)))
		raw.Write(length[:])
		raw.Write(payload)
	}
	return hex.EncodeToString(raw.Bytes())
}

func (s *fingerprintStorage) object(id uint32) CatalogObject {
	o := s.objects.at(id - 1)
	return CatalogObject{Kind: s.name(o.kind), Name: s.name(o.name), Parent: s.name(o.parent), NativeType: sql.NullInt64{Int64: o.native, Valid: o.valid != 0}}
}
func (s *fingerprintStorage) record(id uint32) FingerprintRecord {
	e := s.entries.at(id)
	r := FingerprintRecord{Family: s.name(e.family), Key: s.exactKey(e), Digest: e.digest, Count: e.count, SystemFlag: sql.NullInt64{Int64: e.systemFlag, Valid: e.systemFlagValid}, Owners: make([]CatalogObject, int(e.ownerLen)), Links: make([]CatalogLink, int(e.linkLen))}
	for i := range r.Owners {
		r.Owners[i] = s.object(s.owners.at(e.ownerStart + uint32(i)))
	}
	for i := range r.Links {
		l := s.links.at(e.linkStart + uint32(i))
		r.Links[i] = CatalogLink{From: s.object(l.from), To: s.object(l.to), Field: s.name(l.field)}
	}
	return r
}

// RecordCount returns the number of distinct exact tuples (not multiplicities).
func (f *CatalogFingerprint) RecordCount() int {
	if f == nil {
		return 0
	}
	return int(f.storage.entries.n)
}

// VisitRecords materializes one owned DTO at a time in arrival order. Callers
// must not collect an unbounded second copy. Returning false stops iteration.
func (f *CatalogFingerprint) VisitRecords(visit func(FingerprintRecord) bool) {
	if f == nil || visit == nil {
		return
	}
	for i := uint32(0); i < f.storage.entries.n; i++ {
		if !visit(f.storage.record(i)) {
			return
		}
	}
}

// LookupRecord accepts the unchanged full typed hex identity tuple and family.
// Its owned routing slices may be modified without changing the fingerprint.
func (f *CatalogFingerprint) LookupRecord(family, key string) (FingerprintRecord, bool) {
	if f == nil {
		return FingerprintRecord{}, false
	}
	s := &f.storage
	familyID, ok := s.findName(family)
	if !ok {
		return FingerprintRecord{}, false
	}
	encoded, err := compactFingerprintKey(key, s.findName)
	if err != nil {
		return FingerprintRecord{}, false
	}
	id, ok := s.findRecord(familyID, encoded)
	if !ok {
		return FingerprintRecord{}, false
	}
	return s.record(id - 1), true
}

func addFingerprintRecord(f *CatalogFingerprint, r FingerprintRecord, limit int64) error {
	if f == nil || r.Count == 0 {
		return errors.New("schema: invalid fingerprint record")
	}
	s := &f.storage
	var names []string
	var textBytes uint64
	intern := func(name string) (uint32, bool) {
		if id, ok := s.findName(name); ok {
			return id, true
		}
		for i, v := range names {
			if v == name {
				return s.names.n + uint32(i) + 1, true
			}
		}
		names = append(names, name)
		textBytes += uint64(len(name))
		return s.names.n + uint32(len(names)), true
	}
	family, _ := intern(r.Family)
	key, err := compactFingerprintKey(r.Key, intern)
	if err != nil {
		return err
	}
	if id, ok := s.findRecord(family, key); ok {
		e := s.entries.at(id - 1)
		if r.Family != "RDB$DEPENDENCIES" && r.Family != "RDB$USER_PRIVILEGES" {
			return errors.New("schema: duplicate keyed fingerprint row")
		}
		if e.digest != r.Digest || e.systemFlag != r.SystemFlag.Int64 || e.systemFlagValid != r.SystemFlag.Valid {
			return errors.New("schema: conflicting keyed fingerprint row")
		}
		if uint64(e.count)+uint64(r.Count) > math.MaxUint32 {
			return errors.New("schema: fingerprint multiplicity overflow")
		}
		if f.AccountedBytes > limit {
			return ErrFingerprintLimit
		}
		e.count += r.Count
		s.entries.set(id-1, e)
		return nil
	}
	var objects []fingerprintObject
	object := func(o CatalogObject) uint32 {
		kind, _ := intern(o.Kind)
		name, _ := intern(o.Name)
		parent, _ := intern(o.Parent)
		v := fingerprintObject{kind: kind, name: name, parent: parent, native: o.NativeType.Int64}
		if o.NativeType.Valid {
			v.valid = 1
		}
		if id, ok := fingerprintIndexFind(s.objectIndex, fingerprintObjectHash(v), func(id uint32) bool { return s.objects.at(id-1) == v }); ok {
			return id
		}
		for i, old := range objects {
			if old == v {
				return s.objects.n + uint32(i) + 1
			}
		}
		objects = append(objects, v)
		return s.objects.n + uint32(len(objects))
	}
	owners := make([]uint32, len(r.Owners))
	for i, o := range r.Owners {
		owners[i] = object(o)
	}
	links := make([]fingerprintLink, len(r.Links))
	for i, l := range r.Links {
		from, to := object(l.From), object(l.To)
		field, _ := intern(l.Field)
		links[i] = fingerprintLink{from, to, field}
	}
	counts := []uint64{uint64(s.entries.n) + 1, uint64(s.names.n) + uint64(len(names)), uint64(s.objects.n) + uint64(len(objects)), uint64(s.owners.n) + uint64(len(owners)), uint64(s.links.n) + uint64(len(links)), uint64(s.text.n) + textBytes, uint64(s.keys.n) + uint64(len(key))}
	for _, n := range counts {
		if n > math.MaxUint32 {
			return ErrFingerprintLimit
		}
	}
	retained := fingerprintAllocation(int64(unsafe.Sizeof(*f))) + fingerprintAllocation(int64(len(f.Version)))
	var extra int64
	charge := func(cost, peak int64) { retained += cost; extra += peak }
	charge(s.entries.cost(uint32(counts[0])))
	charge(s.names.cost(uint32(counts[1])))
	charge(s.objects.cost(uint32(counts[2])))
	charge(s.owners.cost(uint32(counts[3])))
	charge(s.links.cost(uint32(counts[4])))
	charge(s.text.cost(uint32(counts[5])))
	charge(s.keys.cost(uint32(counts[6])))
	charge(fingerprintIndexCost(s.nameIndex, uint32(counts[1])))
	charge(fingerprintIndexCost(s.objectIndex, uint32(counts[2])))
	charge(fingerprintIndexCost(s.recordIndex, uint32(counts[0])))
	if limit < 0 || retained > limit || extra > limit-retained {
		return ErrFingerprintLimit
	}
	// Only now may backing capacity, interned values and row facts be retained.
	s.entries.ensure(uint32(counts[0]))
	s.names.ensure(uint32(counts[1]))
	s.objects.ensure(uint32(counts[2]))
	s.owners.ensure(uint32(counts[3]))
	s.links.ensure(uint32(counts[4]))
	s.text.ensure(uint32(counts[5]))
	s.keys.ensure(uint32(counts[6]))
	fingerprintIndexEnsure(&s.nameIndex, uint32(counts[1]), s.nameHash)
	fingerprintIndexEnsure(&s.objectIndex, uint32(counts[2]), func(id uint32) uint64 { return fingerprintObjectHash(s.objects.at(id - 1)) })
	fingerprintIndexEnsure(&s.recordIndex, uint32(counts[0]), func(id uint32) uint64 { return s.recordHash(s.entries.at(id - 1)) })
	for _, name := range names {
		n := fingerprintName{s.text.n, uint32(len(name))}
		for i := 0; i < len(name); i++ {
			s.text.append(name[i])
		}
		s.names.append(n)
		fingerprintIndexInsert(s.nameIndex, fingerprintHashString(name), s.names.n)
	}
	for _, o := range objects {
		s.objects.append(o)
		fingerprintIndexInsert(s.objectIndex, fingerprintObjectHash(o), s.objects.n)
	}
	e := fingerprintEntry{family: family, keyStart: s.keys.n, keyLen: uint32(len(key)), count: r.Count, digest: r.Digest, systemFlag: r.SystemFlag.Int64, systemFlagValid: r.SystemFlag.Valid, ownerStart: s.owners.n, ownerLen: uint32(len(owners)), linkStart: s.links.n, linkLen: uint32(len(links))}
	for _, v := range key {
		s.keys.append(v)
	}
	for _, o := range owners {
		s.owners.append(o)
	}
	for _, l := range links {
		s.links.append(l)
	}
	s.entries.append(e)
	fingerprintIndexInsert(s.recordIndex, s.recordHash(e), s.entries.n)
	if f.AccountedBytes == 0 {
		f.Version = strings.Clone(f.Version)
	}
	f.AccountedBytes = retained
	return nil
}
