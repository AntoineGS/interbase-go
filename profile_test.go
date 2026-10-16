package interbase

import (
	"errors"
	"testing"
)

func TestNormalizeProfileOptionsDefaultsMaxRows(t *testing.T) {
	got, err := normalizeProfileOptions(ProfileOptions{})
	if err != nil {
		t.Fatalf("normalizeProfileOptions(): %v", err)
	}
	if got.MaxRows != 256 {
		t.Fatalf("MaxRows = %d, want 256", got.MaxRows)
	}
}

func TestNormalizeProfileOptionsRejectsNegativeMaxRows(t *testing.T) {
	if _, err := normalizeProfileOptions(ProfileOptions{MaxRows: -1}); err == nil {
		t.Fatal("normalizeProfileOptions() succeeded with a negative MaxRows")
	}
}

func TestFinishProfileSetsActivityDelta(t *testing.T) {
	before := Activity{Tables: map[int]TableCounters{7: {SeqReads: 10}}}
	after := Activity{Tables: map[int]TableCounters{7: {SeqReads: 15, Updates: 2}}}

	got, err := finishProfile(ProfileResult{}, before, after)
	if err != nil {
		t.Fatalf("finishProfile(): %v", err)
	}
	if want := (TableCounters{SeqReads: 5, Updates: 2}); got.Activity.Tables[7] != want {
		t.Fatalf("Activity.Tables[7] = %+v, want %+v", got.Activity.Tables[7], want)
	}
}

func TestFinishProfilePropagatesActivityCounterReset(t *testing.T) {
	before := Activity{Tables: map[int]TableCounters{7: {SeqReads: 10}}}
	after := Activity{Tables: map[int]TableCounters{7: {SeqReads: 4}}}

	_, err := finishProfile(ProfileResult{}, before, after)
	if !errors.Is(err, ErrActivityCounterReset) {
		t.Fatalf("finishProfile() error = %v, want ErrActivityCounterReset", err)
	}
}
