package services

import (
	"errors"
	"testing"
)

func TestNativeServiceCloseRetriesAfterDetachFailure(t *testing.T) {
	wantErr := errors.New("detach failed")
	calls := 0
	service := &nativeService{
		closeOverride: func() error {
			calls++
			if calls == 1 {
				return wantErr
			}
			return nil
		},
	}

	if err := service.close(); !errors.Is(err, wantErr) {
		t.Fatalf("first close error = %v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("first close calls = %d, want 1", calls)
	}

	if err := service.close(); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	if calls != 2 {
		t.Fatalf("detach calls = %d, want 2", calls)
	}
}
