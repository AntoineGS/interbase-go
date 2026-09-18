package events

/*
#cgo linux,amd64 CFLAGS: -I/opt/interbase/include
#cgo linux,amd64 LDFLAGS: -L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds
#include <stdlib.h>
#include "native.h"
*/
import "C"

import (
	"errors"
	"time"
	"unsafe"
)

// nativeTestSubscription is deliberately unexported. It keeps the C callback
// harness available to package tests without adding a public test API.
type nativeTestSubscription struct {
	pointer *C.ib_event_subscription
}

func newTestNativeSubscription(t interface {
	Helper()
	Fatalf(string, ...any)
}, names ...string) *nativeTestSubscription {
	t.Helper()
	cNames := make([]*C.char, len(names))
	for index, name := range names {
		cNames[index] = C.CString(name)
	}
	defer func() {
		for _, name := range cNames {
			C.free(unsafe.Pointer(name))
		}
	}()
	var namesPointer **C.char
	if len(cNames) != 0 {
		namesPointer = (**C.char)(unsafe.Pointer(&cNames[0]))
	}
	var errorPointer *C.char
	pointer := C.ib_event_test_new((**C.char)(namesPointer), C.size_t(len(names)), &errorPointer)
	if pointer == nil {
		t.Fatalf("create native test subscription: %s", takeNativeHarnessError(errorPointer))
	}
	return &nativeTestSubscription{pointer: pointer}
}

func destroyTestNativeSubscription(t interface {
	Helper()
	Errorf(string, ...any)
}, subscription *nativeTestSubscription) {
	t.Helper()
	if subscription == nil || subscription.pointer == nil {
		return
	}
	var errorPointer *C.char
	if result := C.ib_event_destroy(subscription.pointer, &errorPointer); result != 0 {
		t.Errorf("destroy native test subscription: %s", takeNativeHarnessError(errorPointer))
	}
	subscription.pointer = nil
}

func (s *nativeTestSubscription) emit(updated []byte, length int) error {
	if s == nil || s.pointer == nil {
		return errors.New("native test subscription is closed")
	}
	var updatedPointer *C.char
	if len(updated) != 0 {
		updatedPointer = (*C.char)(C.CBytes(updated))
		defer C.free(unsafe.Pointer(updatedPointer))
	}
	var errorPointer *C.char
	if result := C.ib_event_test_emit(s.pointer, updatedPointer, C.short(length), &errorPointer); result != 0 {
		return errors.New(takeNativeHarnessError(errorPointer))
	}
	return nil
}

func (s *nativeTestSubscription) emitCounts(blockIndex int, counts []uint64) error {
	if s == nil || s.pointer == nil {
		return errors.New("native test subscription is closed")
	}
	if blockIndex < 0 || len(counts) == 0 {
		return errors.New("native test counter arguments are invalid")
	}
	var countsPointer *C.uint64_t
	if len(counts) != 0 {
		countsPointer = (*C.uint64_t)(unsafe.Pointer(&counts[0]))
	}
	var errorPointer *C.char
	if result := C.ib_event_test_emit_counts(s.pointer, C.size_t(blockIndex),
		countsPointer, C.size_t(len(counts)), &errorPointer); result != 0 {
		return errors.New(takeNativeHarnessError(errorPointer))
	}
	return nil
}

func (s *nativeTestSubscription) wait(timeout time.Duration) (bool, error) {
	if s == nil || s.pointer == nil {
		return false, errors.New("native test subscription is closed")
	}
	milliseconds := timeout / time.Millisecond
	if timeout%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds < 0 || milliseconds > time.Duration(int64(^uint32(0)>>1)) {
		milliseconds = time.Duration(int64(^uint32(0) >> 1))
	}
	var ready C.int
	var errorPointer *C.char
	if result := C.ib_event_wait(s.pointer, C.int(milliseconds), &ready, &errorPointer); result != 0 {
		return false, errors.New(takeNativeHarnessError(errorPointer))
	}
	return ready != 0, nil
}

func (s *nativeTestSubscription) ready(timeout time.Duration) (bool, error) {
	if s == nil || s.pointer == nil {
		return false, errors.New("native test subscription is closed")
	}
	milliseconds := timeout / time.Millisecond
	if timeout%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds < 0 || milliseconds > time.Duration(int64(^uint32(0)>>1)) {
		milliseconds = time.Duration(int64(^uint32(0) >> 1))
	}
	var ready C.int
	var errorPointer *C.char
	if result := C.ib_event_ready(s.pointer, C.int(milliseconds), &ready, &errorPointer); result != 0 {
		return false, errors.New(takeNativeHarnessError(errorPointer))
	}
	return ready != 0, nil
}

func (s *nativeTestSubscription) take() ([]uint64, bool, error) {
	return s.takeCounts(1)
}

func (s *nativeTestSubscription) takeCounts(count int) ([]uint64, bool, error) {
	if s == nil || s.pointer == nil {
		return nil, false, errors.New("native test subscription is closed")
	}
	if count <= 0 {
		return nil, false, errors.New("native test counter count is invalid")
	}
	counts := make([]C.uint64_t, count)
	var hasCounts C.int
	var errorPointer *C.char
	if result := C.ib_event_take(s.pointer, &counts[0], C.size_t(count),
		&hasCounts, &errorPointer); result != 0 {
		return nil, false, errors.New(takeNativeHarnessError(errorPointer))
	}
	values := make([]uint64, count)
	for index, value := range counts {
		values[index] = uint64(value)
	}
	return values, hasCounts != 0, nil
}

func (s *nativeTestSubscription) stop() error {
	if s == nil || s.pointer == nil {
		return nil
	}
	var errorPointer *C.char
	if result := C.ib_event_stop(s.pointer, &errorPointer); result != 0 {
		return errors.New(takeNativeHarnessError(errorPointer))
	}
	return nil
}

func (s *nativeTestSubscription) destroy() error {
	if s == nil || s.pointer == nil {
		return nil
	}
	pointer := s.pointer
	var errorPointer *C.char
	if result := C.ib_event_destroy(pointer, &errorPointer); result != 0 {
		return errors.New(takeNativeHarnessError(errorPointer))
	}
	s.pointer = nil
	return nil
}

func (s *nativeTestSubscription) holdCallback() error {
	if s == nil || s.pointer == nil {
		return errors.New("native test subscription is closed")
	}
	var errorPointer *C.char
	if result := C.ib_event_test_hold_callback(s.pointer, &errorPointer); result != 0 {
		return errors.New(takeNativeHarnessError(errorPointer))
	}
	return nil
}

func (s *nativeTestSubscription) waitCallback(timeout time.Duration) (bool, error) {
	if s == nil || s.pointer == nil {
		return false, errors.New("native test subscription is closed")
	}
	milliseconds := timeout / time.Millisecond
	if timeout%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds < 0 || milliseconds > time.Duration(int64(^uint32(0)>>1)) {
		milliseconds = time.Duration(int64(^uint32(0) >> 1))
	}
	var entered C.int
	var errorPointer *C.char
	if result := C.ib_event_test_wait_callback(s.pointer, C.int(milliseconds), &entered, &errorPointer); result != 0 {
		return false, errors.New(takeNativeHarnessError(errorPointer))
	}
	return entered != 0, nil
}

func (s *nativeTestSubscription) releaseCallback() error {
	if s == nil || s.pointer == nil {
		return errors.New("native test subscription is closed")
	}
	var errorPointer *C.char
	if result := C.ib_event_test_release_callback(s.pointer, &errorPointer); result != 0 {
		return errors.New(takeNativeHarnessError(errorPointer))
	}
	return nil
}

func takeNativeHarnessError(errorPointer *C.char) string {
	if errorPointer == nil {
		return "native test operation failed"
	}
	message := C.GoString(errorPointer)
	C.ib_event_error_free(errorPointer)
	return message
}
