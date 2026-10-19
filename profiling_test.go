package interbase

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"interbase-go/internal/nativegate"
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
	native.databaseInfoItemsOverride = perCodeInfoItems(func(code byte) ([]byte, error) {
		calls[code]++
		payload, ok := responses[code]
		if !ok {
			return nil, errors.New("unexpected database info code")
		}
		return introspectionInfoPayload(code, payload), nil
	})
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
	native.databaseInfoItemsOverride = perCodeInfoItems(func(code byte) ([]byte, error) {
		if code == 27 {
			return []byte{infoEnd}, nil
		}
		if code >= 5 && code <= 8 {
			return introspectionInfoPayload(code, []byte{0}), nil
		}
		return introspectionInfoPayload(code, nil), nil
	})
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
	native.databaseInfoItemsOverride = perCodeInfoItems(func(code byte) ([]byte, error) {
		if code == 30 {
			cancel()
			return []byte{infoEnd}, nil
		}
		if code >= 5 && code <= 8 {
			return introspectionInfoPayload(code, []byte{0}), nil
		}
		return introspectionInfoPayload(code, nil), nil
	})
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

func TestActivityCellInt64AcceptsIntegralFloat64Counters(t *testing.T) {
	for _, tt := range []struct {
		value float64
		want  int64
	}{
		{value: 0, want: 0},
		{value: 42, want: 42},
		{value: -7, want: -7},
		{value: -0x1p63, want: -1 << 63},
	} {
		got, err := activityCellInt64(Cell{Value: tt.value})
		if err != nil || got != tt.want {
			t.Errorf("activityCellInt64(%v) = (%d, %v), want (%d, nil)", tt.value, got, err, tt.want)
		}
	}
}

func TestActivityCellInt64RejectsInvalidFloat64Counters(t *testing.T) {
	for _, value := range []float64{1.5, math.NaN(), math.Inf(1), math.Inf(-1), 0x1p63, -0x1p63 - 2048} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			if _, err := activityCellInt64(Cell{Value: value}); err == nil {
				t.Fatalf("activityCellInt64(%v) error = nil, want error", value)
			}
		})
	}
}

// perCodeInfoItems adapts a per-code responder (a full single-item frame, or a
// bare isc_info_end for an omitted counter) to the batched native seam.
func perCodeInfoItems(respond func(byte) ([]byte, error)) func([]byte) ([]byte, error) {
	return func(codes []byte) ([]byte, error) {
		var response []byte
		for _, code := range codes {
			frame, err := respond(code)
			if err != nil {
				return nil, err
			}
			if len(frame) == 1 && frame[0] == infoEnd {
				continue
			}
			response = append(response, frame[:len(frame)-1]...)
		}
		return append(response, infoEnd), nil
	}
}

var activityTestCodes = []byte{5, 6, 7, 8, 23, 24, 25, 26, 27, 28, 29, 30}

func infoItemsFrame(items ...[]byte) []byte {
	var response []byte
	for _, item := range items {
		response = append(response, item...)
	}
	return append(response, infoEnd)
}

func infoItemRecord(code byte, data ...byte) []byte {
	return append([]byte{code, byte(len(data)), byte(len(data) >> 8)}, data...)
}

func TestParseInfoItemsReadsAllTwelveActivityItems(t *testing.T) {
	var records [][]byte
	want := make(map[byte]InfoItem)
	for index, code := range activityTestCodes {
		data := []byte{byte(index), 0, 0, 0}
		records = append(records, infoItemRecord(code, data...))
		want[code] = InfoItem{Code: code, Data: data}
	}
	got, err := parseInfoItems(infoItemsFrame(records...), activityTestCodes)
	if err != nil {
		t.Fatalf("parseInfoItems() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseInfoItems() = %#v, want %#v", got, want)
	}
}

func TestParseInfoItemsOmitsMissingItems(t *testing.T) {
	got, err := parseInfoItems(infoItemsFrame(infoItemRecord(5, 1, 0, 0, 0), infoItemRecord(30, 18, 0, 9, 0, 0, 0)), activityTestCodes)
	if err != nil {
		t.Fatalf("parseInfoItems() error = %v", err)
	}
	if len(got) != 2 || got[5].Data[0] != 1 || len(got[30].Data) != 6 {
		t.Fatalf("parseInfoItems() = %#v, want only items 5 and 30", got)
	}
	if _, present := got[6]; present {
		t.Fatal("parseInfoItems() invented a missing item")
	}
	empty, err := parseInfoItems([]byte{infoEnd}, activityTestCodes)
	if err != nil || len(empty) != 0 {
		t.Fatalf("parseInfoItems(end only) = (%#v, %v), want empty map", empty, err)
	}
}

func TestParseInfoItemsReportsTruncation(t *testing.T) {
	for name, response := range map[string][]byte{
		"marker only":       {infoTruncated},
		"after a full item": append(infoItemRecord(5, 1, 0, 0, 0), infoTruncated),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseInfoItems(response, activityTestCodes); !errors.Is(err, errInfoTruncated) {
				t.Fatalf("parseInfoItems() error = %v, want errInfoTruncated", err)
			}
		})
	}
}

func TestParseInfoItemsRejectsMalformedResponses(t *testing.T) {
	for name, response := range map[string][]byte{
		"empty":                nil,
		"length beyond buffer": {5, 0xff, 0x7f, 1, 0, infoEnd},
		"short header":         {5, 4},
		"no terminator":        infoItemRecord(5, 1, 0, 0, 0),
		"trailing data":        append(infoItemsFrame(infoItemRecord(5, 1)), 0),
		"unrequested code":     infoItemsFrame(infoItemRecord(99, 1)),
		"duplicate code":       infoItemsFrame(infoItemRecord(5, 1), infoItemRecord(5, 2)),
		"error item":           infoItemsFrame(infoItemRecord(infoError, 1, 0)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseInfoItems(response, activityTestCodes); err == nil || errors.Is(err, errInfoTruncated) {
				t.Fatalf("parseInfoItems() error = %v, want a malformed-response error", err)
			}
		})
	}
}

func TestReadActivityIssuesOneNativeInfoCall(t *testing.T) {
	var calls [][]byte
	native := introspectionTestNative(3)
	native.databaseInfoItemsOverride = func(codes []byte) ([]byte, error) {
		calls = append(calls, append([]byte(nil), codes...))
		return infoItemsFrame(infoItemRecord(5, 10, 0, 0, 0), infoItemRecord(23, 17, 0, 100, 0, 0, 0)), nil
	}
	native.databaseInfoOverride = func(byte) ([]byte, error) {
		return nil, errors.New("single-item info must not be used for activity")
	}
	c := &conn{native: native}
	got, err := c.readActivity(context.Background())
	if err != nil {
		t.Fatalf("readActivity() error = %v", err)
	}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], activityTestCodes) {
		t.Fatalf("native info calls = %v, want exactly one for %v", calls, activityTestCodes)
	}
	if got.Pages.Reads != 10 || got.Tables[17].SeqReads != 100 {
		t.Fatalf("readActivity() = %#v, want page reads 10 and relation 17 seq reads 100", got)
	}
}

func TestReadActivityFallsBackToSingleItemsWhenResponseExceedsNativeLimit(t *testing.T) {
	var calls int
	native := introspectionTestNative(3)
	native.databaseInfoItemsOverride = func(codes []byte) ([]byte, error) {
		calls++
		if len(codes) > 1 {
			return nil, errInfoResponseTooLarge
		}
		if codes[0] == 23 {
			return infoItemsFrame(infoItemRecord(23, 17, 0, 100, 0, 0, 0)), nil
		}
		return []byte{infoEnd}, nil
	}
	got, err := (&conn{native: native}).readActivity(context.Background())
	if err != nil {
		t.Fatalf("readActivity() error = %v", err)
	}
	if calls != 1+len(activityTestCodes) {
		t.Fatalf("native info calls = %d, want one batch plus %d single-item calls", calls, len(activityTestCodes))
	}
	if want := (TableCounters{SeqReads: 100}); got.Tables[17] != want {
		t.Fatalf("relation 17 = %#v, want %#v", got.Tables[17], want)
	}
}

func TestProbeRoundTripIssuesOneSingleItemCallAndTimesIt(t *testing.T) {
	var calls [][]byte
	native := introspectionTestNative(3)
	native.databaseInfoItemsOverride = func(codes []byte) ([]byte, error) {
		calls = append(calls, append([]byte(nil), codes...))
		time.Sleep(5 * time.Millisecond)
		return infoItemsFrame(infoItemRecord(5, 1, 0, 0, 0)), nil
	}
	elapsed, err := (&conn{native: native}).probeRoundTrip(context.Background())
	if err != nil {
		t.Fatalf("probeRoundTrip() error = %v", err)
	}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], []byte{5}) {
		t.Fatalf("native info calls = %v, want one call for isc_info_reads", calls)
	}
	if elapsed < 5*time.Millisecond {
		t.Fatalf("probeRoundTrip() = %v, want at least the 5ms native call", elapsed)
	}
}

func TestProbeRoundTripReturnsNativeError(t *testing.T) {
	native := introspectionTestNative(3)
	native.databaseInfoItemsOverride = func([]byte) ([]byte, error) { return nil, errors.New("link down") }
	if _, err := (&conn{native: native}).probeRoundTrip(context.Background()); err == nil {
		t.Fatal("probeRoundTrip() error = nil, want native error")
	}
}

func TestReadActivityRemembersOversizedBatchAndReadsPerItem(t *testing.T) {
	var calls int
	native := introspectionTestNative(3)
	native.databaseInfoItemsOverride = func(codes []byte) ([]byte, error) {
		calls++
		if len(codes) > 1 {
			return nil, errInfoResponseTooLarge
		}
		return []byte{infoEnd}, nil
	}
	c := &conn{native: native}
	for snapshot := 0; snapshot < 3; snapshot++ {
		if _, err := c.readActivity(context.Background()); err != nil {
			t.Fatalf("readActivity() #%d error = %v", snapshot+1, err)
		}
	}
	if want := 1 + 3*len(activityTestCodes); calls != want {
		t.Fatalf("native info calls = %d, want %d (one rejected batch, then only per-item reads)", calls, want)
	}
}

func TestReadActivityKeepsBatchingAfterTransientNonSizeError(t *testing.T) {
	var batches int
	native := introspectionTestNative(3)
	native.databaseInfoItemsOverride = func(codes []byte) ([]byte, error) {
		if len(codes) > 1 {
			batches++
			if batches == 1 {
				return nil, errors.New("link down")
			}
		}
		return infoItemsFrame(infoItemRecord(5, 1, 0, 0, 0)), nil
	}
	c := &conn{native: native}
	if _, err := c.readActivity(context.Background()); err == nil {
		t.Fatal("readActivity() error = nil, want the link error")
	}
	if _, err := c.readActivity(context.Background()); err != nil {
		t.Fatalf("readActivity() after transient error = %v", err)
	}
	if batches != 2 {
		t.Fatalf("batch calls = %d, want 2 (a non-size error must not disable batching)", batches)
	}
}

func TestDatabaseInfoItemsTimedExcludesGateAdmission(t *testing.T) {
	native := introspectionTestNative(3)
	native.databaseInfoItemsOverride = func([]byte) ([]byte, error) {
		time.Sleep(5 * time.Millisecond)
		return infoItemsFrame(infoItemRecord(5, 1, 0, 0, 0)), nil
	}
	releaseExclusive := nativegate.Global.EnterExclusive()
	const hold = 80 * time.Millisecond
	time.AfterFunc(hold, releaseExclusive)
	started := time.Now()
	_, elapsed, err := native.databaseInfoItemsTimed([]byte{5})
	if err != nil {
		t.Fatalf("databaseInfoItemsTimed() error = %v", err)
	}
	if total := time.Since(started); total < hold {
		t.Fatalf("call returned after %v, before the gate was released", total)
	}
	if elapsed < 5*time.Millisecond || elapsed >= hold/2 {
		t.Fatalf("timed duration = %v, want only the ~5ms native call, not the gate wait", elapsed)
	}
}
