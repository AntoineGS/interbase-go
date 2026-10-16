package interbase

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

func TestParseRelationCountsDecodesThreeEntries(t *testing.T) {
	payload := []byte{
		7, 0, 12, 0, 0, 0,
		8, 0, 0x34, 0x12, 0, 0,
		9, 0, 255, 255, 255, 255,
	}

	got, err := parseRelationCounts(payload)
	if err != nil {
		t.Fatalf("parseRelationCounts() error = %v", err)
	}
	want := map[int]int64{7: 12, 8: 0x1234, 9: 0xffffffff}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseRelationCounts() = %#v, want %#v", got, want)
	}
}

func TestParseRelationCountsAcceptsEmptyPayload(t *testing.T) {
	got, err := parseRelationCounts(nil)
	if err != nil {
		t.Fatalf("parseRelationCounts(nil) error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("parseRelationCounts(nil) = %#v, want empty map", got)
	}
}

func TestParseRelationCountsRejectsIncompleteEntry(t *testing.T) {
	if _, err := parseRelationCounts([]byte{1, 0, 2}); err == nil {
		t.Fatal("parseRelationCounts() succeeded for an incomplete relation/count pair")
	}
}

func TestParseRelationCountsDecodesTwoThousandEntries(t *testing.T) {
	payload := make([]byte, 2000*6)
	for index := 0; index < 2000; index++ {
		entry := payload[index*6 : (index+1)*6]
		binary.LittleEndian.PutUint16(entry[:2], uint16(index))
		binary.LittleEndian.PutUint32(entry[2:], uint32(index*3))
	}

	got, err := parseRelationCounts(payload)
	if err != nil {
		t.Fatalf("parseRelationCounts() error = %v", err)
	}
	if len(got) != 2000 {
		t.Fatalf("decoded relations = %d, want 2000", len(got))
	}
	for _, index := range []int{0, 1, 999, 1999} {
		if got[index] != int64(index*3) {
			t.Errorf("relation %d count = %d, want %d", index, got[index], index*3)
		}
	}
}

func TestActivityDeltaUsesZeroForNewRelationsAndOmitsUnchangedCounters(t *testing.T) {
	before := Activity{Tables: map[int]TableCounters{
		7: {SeqReads: 4, IdxReads: 9},
		8: {},
	}}
	after := Activity{Tables: map[int]TableCounters{
		7: {SeqReads: 10, IdxReads: 9, Inserts: 2},
		8: {},
		9: {SeqReads: 5, Deletes: 1},
	}}

	got, err := after.Delta(before)
	if err != nil {
		t.Fatalf("Activity.Delta() error = %v", err)
	}
	want := ActivityDelta{Tables: map[int]TableCounters{
		7: {SeqReads: 6, Inserts: 2},
		9: {SeqReads: 5, Deletes: 1},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Activity.Delta() = %#v, want %#v", got, want)
	}
}

func TestActivityDeltaRejectsDecreasedCounters(t *testing.T) {
	before := Activity{Tables: map[int]TableCounters{7: {SeqReads: 5}}}
	after := Activity{Tables: map[int]TableCounters{7: {SeqReads: 2}}}
	if _, err := after.Delta(before); !errors.Is(err, ErrActivityCounterReset) {
		t.Fatalf("Activity.Delta() error = %v, want ErrActivityCounterReset", err)
	}
}

func TestActivityDeltaKeepsPagesNilAndSubtractsAvailablePages(t *testing.T) {
	withoutPages, err := (Activity{}).Delta(Activity{})
	if err != nil {
		t.Fatalf("Activity.Delta() with nil pages error = %v", err)
	}
	if withoutPages.Pages != nil {
		t.Fatalf("Activity.Delta().Pages = %#v, want nil", withoutPages.Pages)
	}

	got, err := (Activity{Pages: &PageCounters{Reads: 8, Fetches: 12}}).Delta(
		Activity{Pages: &PageCounters{Reads: 3, Fetches: 12}})
	if err != nil {
		t.Fatalf("Activity.Delta() with pages error = %v", err)
	}
	if want := (&PageCounters{Reads: 5}); !reflect.DeepEqual(got.Pages, want) {
		t.Fatalf("Activity.Delta().Pages = %#v, want %#v", got.Pages, want)
	}
}

func TestActivityDeltaAddSumsRelationsAndPages(t *testing.T) {
	first := ActivityDelta{
		Tables: map[int]TableCounters{7: {SeqReads: 3}},
		Pages:  &PageCounters{Reads: 2},
	}
	second := ActivityDelta{
		Tables: map[int]TableCounters{7: {SeqReads: 4}, 9: {Updates: 1}},
		Pages:  &PageCounters{Writes: 5},
	}

	got := first.Add(second)
	want := ActivityDelta{
		Tables: map[int]TableCounters{7: {SeqReads: 7}, 9: {Updates: 1}},
		Pages:  &PageCounters{Reads: 2, Writes: 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ActivityDelta.Add() = %#v, want %#v", got, want)
	}
}

func TestReadActivityDecodesPageAndRelationCounters(t *testing.T) {
	responses := map[byte][]byte{
		5:  {10, 0, 0, 0},
		6:  {11, 0, 0, 0},
		7:  {12, 0, 0, 0},
		8:  {13, 0, 0, 0},
		23: {17, 0, 100, 0, 0, 0, 18, 0, 200, 0, 0, 0},
		24: {17, 0, 3, 0, 0, 0},
		25: {18, 0, 4, 0, 0, 0},
		26: {18, 0, 5, 0, 0, 0},
		27: {18, 0, 6, 0, 0, 0},
		28: {18, 0, 7, 0, 0, 0},
		29: {18, 0, 8, 0, 0, 0},
		30: {18, 0, 9, 0, 0, 0},
	}
	calls := make(map[byte]int)
	native := introspectionTestNative(3)
	native.databaseInfoOverride = func(code byte) ([]byte, error) {
		calls[code]++
		payload, ok := responses[code]
		if !ok {
			return nil, errors.New("unexpected database info code")
		}
		return introspectionInfoPayload(code, payload), nil
	}
	db := openDatabaseSQLTestDB(t, &conn{native: native})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn() error = %v", err)
	}
	defer pooled.Close()

	got, err := ReadActivity(context.Background(), pooled)
	if err != nil {
		t.Fatalf("ReadActivity() error = %v", err)
	}
	want := Activity{
		Tables: map[int]TableCounters{
			17: {SeqReads: 100, IdxReads: 3},
			18: {SeqReads: 200, Inserts: 4, Updates: 5, Deletes: 6, Backouts: 7, Purges: 8, Expunges: 9},
		},
		Pages: &PageCounters{Reads: 10, Writes: 11, Fetches: 12, Marks: 13},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadActivity() = %#v, want %#v", got, want)
	}
	for code := byte(5); code <= 8; code++ {
		if calls[code] != 1 {
			t.Errorf("databaseInfo(%d) calls = %d, want 1", code, calls[code])
		}
	}
	for code := byte(23); code <= 30; code++ {
		if calls[code] != 1 {
			t.Errorf("databaseInfo(%d) calls = %d, want 1", code, calls[code])
		}
	}
}

func TestReadActivityTreatsOmittedInfoCounterAsZero(t *testing.T) {
	native := introspectionTestNative(3)
	native.databaseInfoOverride = func(code byte) ([]byte, error) {
		if code == 27 {
			return []byte{infoEnd}, nil
		}
		if code >= 5 && code <= 8 {
			return introspectionInfoPayload(code, []byte{0}), nil
		}
		return introspectionInfoPayload(code, nil), nil
	}
	db := openDatabaseSQLTestDB(t, &conn{native: native})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn() error = %v", err)
	}
	defer pooled.Close()

	got, err := ReadActivity(context.Background(), pooled)
	if err != nil {
		t.Fatalf("ReadActivity() error = %v", err)
	}
	if got.Tables[0].Deletes != 0 || len(got.Tables) != 0 {
		t.Fatalf("ReadActivity().Tables = %#v, want no relation entries for omitted zero counter", got.Tables)
	}
	if want := (&PageCounters{}); !reflect.DeepEqual(got.Pages, want) {
		t.Fatalf("ReadActivity().Pages = %#v, want %#v", got.Pages, want)
	}
}

func TestReadActivityReturnsCancellationAfterOmittedFinalCounter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	native := introspectionTestNative(3)
	native.databaseInfoOverride = func(code byte) ([]byte, error) {
		if code == 30 {
			cancel()
			return []byte{infoEnd}, nil
		}
		if code >= 5 && code <= 8 {
			return introspectionInfoPayload(code, []byte{0}), nil
		}
		return introspectionInfoPayload(code, nil), nil
	}
	db := openDatabaseSQLTestDB(t, &conn{native: native})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn() error = %v", err)
	}
	defer pooled.Close()

	if _, err := ReadActivity(ctx, pooled); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadActivity() error = %v, want context.Canceled", err)
	}
}

func TestRoutineActivityDeltaReportsNewAndChangedRoutinesOnly(t *testing.T) {
	changed := RoutineKey{Kind: "procedure", Name: "P"}
	unchanged := RoutineKey{Kind: "trigger", Name: "T", Relation: "R"}
	newRoutine := RoutineKey{Kind: "procedure", Name: "Q"}
	before := RoutineActivity{Routines: map[RoutineKey]RoutineCounters{
		changed:   {Invocations: 2, PageReads: 8},
		unchanged: {Invocations: 3},
	}}
	after := RoutineActivity{Routines: map[RoutineKey]RoutineCounters{
		changed:    {Invocations: 4, PageReads: 8},
		unchanged:  {Invocations: 3},
		newRoutine: {Invocations: 1},
	}}

	got := after.Delta(before)
	want := map[RoutineKey]RoutineCounters{
		changed:    {Invocations: 2},
		newRoutine: {Invocations: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RoutineActivity.Delta() = %#v, want %#v", got, want)
	}
}
