package services

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
	"sync"
	"unsafe"

	"interbase-go/internal/nativegate"
)

type nativeBackend interface {
	start(request []byte) error
	query(item byte, capacity int) ([]byte, bool, error)
	close() error
}

// nativeService serializes access to one client-library service attachment.
// The client API permits calls from different Go goroutines; the mutex keeps
// the C handle and detach operation from being used concurrently.
type nativeService struct {
	mu            sync.Mutex
	pointer       *C.ib_service_manager
	closeOverride func() error
}

func openNativeService(target string, spb []byte) (*nativeService, error) {
	return openNativeServiceContext(context.Background(), target, spb)
}

func openNativeServiceContext(ctx context.Context, target string, spb []byte) (*nativeService, error) {
	release, err := nativegate.Global.EnterExclusiveContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pointer, err := openNativeServiceNative(target, spb)
	if pointer == nil {
		return nil, err
	}
	return &nativeService{pointer: pointer}, err
}

func (s *nativeService) start(request []byte) error {
	if s == nil {
		return ErrClosed
	}
	release := nativegate.Global.Enter()
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pointer == nil {
		return ErrClosed
	}
	return startNativeService(s.pointer, request)
}

func (s *nativeService) query(item byte, capacity int) ([]byte, bool, error) {
	if s == nil {
		return nil, false, ErrClosed
	}
	release := nativegate.Global.Enter()
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pointer == nil {
		return nil, false, ErrClosed
	}
	return queryNativeService(s.pointer, item, capacity)
}

func (s *nativeService) close() error {
	if s == nil {
		return nil
	}
	release := nativegate.Global.EnterExclusive()
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pointer == nil && s.closeOverride == nil {
		return nil
	}
	if s.closeOverride != nil {
		if err := s.closeOverride(); err != nil {
			return err
		}
		s.pointer = nil
		return nil
	}
	if err := closeNativeService(s.pointer); err != nil {
		return err
	}
	s.pointer = nil
	return nil
}

func openNativeServiceNative(target string, spb []byte) (*C.ib_service_manager, error) {
	ctarget := C.CString(target)
	defer C.free(unsafe.Pointer(ctarget))
	var spbPointer *C.uchar
	if len(spb) != 0 {
		spbPointer = (*C.uchar)(C.CBytes(spb))
		defer C.free(unsafe.Pointer(spbPointer))
	}
	var errorPointer *C.char
	var failed C.int
	pointer := C.ib_service_open(ctarget, C.size_t(len(target)), spbPointer,
		C.size_t(len(spb)), &errorPointer, &failed)
	if pointer == nil {
		return nil, takeServiceNativeError(errorPointer)
	}
	if failed != 0 || errorPointer != nil {
		return pointer, takeServiceNativeError(errorPointer)
	}
	return pointer, nil
}

func startNativeService(pointer *C.ib_service_manager, request []byte) error {
	var requestPointer *C.uchar
	if len(request) != 0 {
		requestPointer = (*C.uchar)(C.CBytes(request))
		defer C.free(unsafe.Pointer(requestPointer))
	}
	var errorPointer *C.char
	if result := C.ib_service_start(pointer, requestPointer, C.size_t(len(request)), &errorPointer); result != 0 {
		return takeServiceNativeError(errorPointer)
	}
	return nil
}

func queryNativeService(pointer *C.ib_service_manager, item byte, capacity int) ([]byte, bool, error) {
	var resultPointer *C.uchar
	var resultLength C.size_t
	var truncated C.int
	var errorPointer *C.char
	if result := C.ib_service_query(pointer, C.uchar(item), C.size_t(capacity),
		&resultPointer, &resultLength, &truncated, &errorPointer); result != 0 {
		return nil, false, takeServiceNativeError(errorPointer)
	}
	if resultPointer == nil {
		return nil, false, errors.New("interbase services: native query returned no buffer")
	}
	defer C.ib_service_buffer_free(resultPointer)
	data := C.GoBytes(unsafe.Pointer(resultPointer), C.int(resultLength))
	return data, truncated != 0, nil
}

func closeNativeService(pointer *C.ib_service_manager) error {
	var errorPointer *C.char
	if result := C.ib_service_close(pointer, &errorPointer); result != 0 {
		return takeServiceNativeError(errorPointer)
	}
	return nil
}

func takeServiceNativeError(errorPointer *C.char) error {
	if errorPointer == nil {
		return errors.New("interbase services: native operation failed")
	}
	message := C.GoString(errorPointer)
	C.ib_service_error_free(errorPointer)
	if message == "" {
		return errors.New("interbase services: native operation failed")
	}
	return errors.New(message)
}
