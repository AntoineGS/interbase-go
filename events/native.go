package events

/*
#cgo linux,amd64 CFLAGS: -I/opt/interbase/include
#cgo linux,amd64 LDFLAGS: -L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds
#include <stdlib.h>
#include "native.h"
*/
import "C"

import (
	"context"
	"errors"
	"time"
	"unsafe"

	"interbase-go/internal/nativegate"
)

type nativeBackend interface {
	ready(context.Context, time.Duration) (bool, error)
	wait(context.Context, time.Duration) (bool, error)
	take(context.Context, int) ([]uint64, bool, error)
	stop() error
	destroy() error
}

type nativeSubscription struct {
	pointer *C.ib_event_subscription
}

func openNative(ctx context.Context, cfg Config, attachment, charset string,
	dialect int, names []string) (*nativeSubscription, error) {
	if ctx == nil {
		return nil, errors.New("interbase events: nil context")
	}
	connectTimeout, err := normalizeConnectTimeout(cfg.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	database := C.CString(attachment)
	defer C.free(unsafe.Pointer(database))
	user := C.CString(cfg.User)
	defer C.free(unsafe.Pointer(user))
	password := C.CString(cfg.Password)
	defer C.free(unsafe.Pointer(password))
	role := C.CString(cfg.Role)
	defer C.free(unsafe.Pointer(role))
	encryptedPassword := C.CString(cfg.EncryptedPassword)
	defer C.free(unsafe.Pointer(encryptedPassword))
	systemEncryptionPassword := C.CString(cfg.SystemEncryptionPassword)
	defer C.free(unsafe.Pointer(systemEncryptionPassword))
	charsetName := C.CString(charset)
	defer C.free(unsafe.Pointer(charsetName))

	cNames := make([]*C.char, len(names))
	for index, name := range names {
		cNames[index] = C.CString(name)
	}
	defer func() {
		for _, name := range cNames {
			C.free(unsafe.Pointer(name))
		}
	}()

	var cNamesPointer **C.char
	if len(cNames) != 0 {
		cNamesPointer = (**C.char)(unsafe.Pointer(&cNames[0]))
	}

	/*
	 * The C boundary admits each individual client-library operation.  Do not
	 * retain this ordinary admission over ib_event_open: its failure cleanup
	 * may need the exclusive cancel/detach phase, and holding the ordinary
	 * slot here would make that cleanup wait on itself.  This context-aware
	 * admission still makes Subscribe cancellation observable while it waits
	 * for an already-active lifecycle operation; the C bridge re-admits the
	 * actual attach/block/queue calls after this preflight.
	 */
	release, err := nativegate.Global.EnterContext(ctx)
	if err != nil {
		return nil, err
	}
	release()
	var errorPointer *C.char
	pointer := C.ib_event_open(
		database, C.size_t(len(attachment)),
		user, C.size_t(len(cfg.User)),
		password, C.size_t(len(cfg.Password)),
		role, C.size_t(len(cfg.Role)),
		encryptedPassword, C.size_t(len(cfg.EncryptedPassword)),
		systemEncryptionPassword, C.size_t(len(cfg.SystemEncryptionPassword)),
		charsetName, C.size_t(len(charset)), C.int(dialect),
		C.uint32_t(uint64(connectTimeout/time.Second)),
		(**C.char)(cNamesPointer), C.size_t(len(names)), &errorPointer,
	)
	if pointer == nil {
		return nil, takeNativeError(errorPointer)
	}
	return &nativeSubscription{pointer: pointer}, nil
}

func (s *nativeSubscription) wait(ctx context.Context, timeout time.Duration) (bool, error) {
	release, err := nativegate.Global.EnterContext(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	if s == nil || s.pointer == nil {
		return false, ErrClosed
	}
	if timeout < 0 {
		timeout = 0
	}
	milliseconds := timeout / time.Millisecond
	if timeout%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds > time.Duration(int64(^uint32(0)>>1)) {
		milliseconds = time.Duration(int64(^uint32(0) >> 1))
	}
	var ready C.int
	var errorPointer *C.char
	if result := C.ib_event_wait(s.pointer, C.int(milliseconds), &ready, &errorPointer); result != 0 {
		return false, takeNativeError(errorPointer)
	}
	return ready != 0, nil
}

func (s *nativeSubscription) ready(ctx context.Context, timeout time.Duration) (bool, error) {
	release, err := nativegate.Global.EnterContext(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	if s == nil || s.pointer == nil {
		return false, ErrClosed
	}
	if timeout < 0 {
		timeout = 0
	}
	milliseconds := timeout / time.Millisecond
	if timeout%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds > time.Duration(int64(^uint32(0)>>1)) {
		milliseconds = time.Duration(int64(^uint32(0) >> 1))
	}
	var ready C.int
	var errorPointer *C.char
	if result := C.ib_event_ready(s.pointer, C.int(milliseconds), &ready, &errorPointer); result != 0 {
		return false, takeNativeError(errorPointer)
	}
	return ready != 0, nil
}

func (s *nativeSubscription) take(ctx context.Context, count int) ([]uint64, bool, error) {
	release, err := nativegate.Global.EnterContext(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	if s == nil || s.pointer == nil {
		return nil, false, ErrClosed
	}
	if count <= 0 {
		return nil, false, errors.New("interbase events: event name count is invalid")
	}
	values := make([]C.uint64_t, count)
	var hasCounts C.int
	var errorPointer *C.char
	if result := C.ib_event_take(s.pointer, &values[0], C.size_t(count), &hasCounts, &errorPointer); result != 0 {
		return nil, false, takeNativeError(errorPointer)
	}
	counts := make([]uint64, count)
	for index, value := range values {
		counts[index] = uint64(value)
	}
	return counts, hasCounts != 0, nil
}

func (s *nativeSubscription) stop() error {
	// ib_event_stop marks closing and drains callbacks before its C bridge
	// enters the exclusive gate for each native cancel. Keep this composite
	// operation outside the Go gate or a callback could wait for ordinary
	// admission while stop waits for that callback.
	if s == nil || s.pointer == nil {
		return nil
	}
	var errorPointer *C.char
	if result := C.ib_event_stop(s.pointer, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (s *nativeSubscription) destroy() error {
	// ib_event_destroy reuses the C stop protocol and takes exclusive
	// admission only around the final native detach.
	if s == nil || s.pointer == nil {
		return nil
	}
	pointer := s.pointer
	var errorPointer *C.char
	if result := C.ib_event_destroy(pointer, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	s.pointer = nil
	return nil
}

func (s *nativeSubscription) quarantine() error {
	if s == nil || s.pointer == nil {
		return nil
	}
	var errorPointer *C.char
	if result := C.ib_event_quarantine(s.pointer, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	s.pointer = nil
	return nil
}

func takeNativeError(errorPointer *C.char) error {
	if errorPointer == nil {
		return errors.New("interbase events: native operation failed")
	}
	message := C.GoString(errorPointer)
	C.ib_event_error_free(errorPointer)
	if message == "" {
		return errors.New("interbase events: native operation failed")
	}
	return errors.New(message)
}
