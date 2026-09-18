package events

/*
#cgo linux,amd64 CFLAGS: -I/opt/interbase/include
#cgo linux,amd64 LDFLAGS: -L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds
#include "native.h"
*/
import "C"

import (
	"runtime/cgo"

	"interbase-go/internal/nativegate"
)

// The native callback runs on a client-library thread, so it cannot carry a
// Go closure or a Go pointer into C.  A cgo.Handle gives C an integer token;
// the corresponding release callback deletes the handle after running it.
// The C event implementation owns the enter/leave pairing and uses the
// standalone harness stubs when native.c is linked without this package.

//export ib_event_gate_enter
func ib_event_gate_enter() C.uintptr_t {
	return C.uintptr_t(cgo.NewHandle(nativegate.Global.Enter()))
}

//export ib_event_gate_leave
func ib_event_gate_leave(token C.uintptr_t) {
	releaseGateHandle(token)
}

//export ib_event_gate_enter_exclusive
func ib_event_gate_enter_exclusive() C.uintptr_t {
	return C.uintptr_t(cgo.NewHandle(nativegate.Global.EnterExclusive()))
}

//export ib_event_gate_leave_exclusive
func ib_event_gate_leave_exclusive(token C.uintptr_t) {
	releaseGateHandle(token)
}

func releaseGateHandle(token C.uintptr_t) {
	if token == 0 {
		return
	}
	handle := cgo.Handle(token)
	release, ok := handle.Value().(func())
	if !ok {
		handle.Delete()
		return
	}
	release()
	handle.Delete()
}
