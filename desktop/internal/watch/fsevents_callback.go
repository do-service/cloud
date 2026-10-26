//go:build darwin && cgo

package watch

/*
#include "fsevents.h"
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"
)

// watchGoCallback receives one FSEvents batch from cs_callback. The C
// memory is freed when it returns, so everything is copied out first.
//
//export watchGoCallback
func watchGoCallback(handle C.uintptr_t, events *C.cs_event, n C.size_t) {
	w, ok := cgo.Handle(handle).Value().(*fsevents)
	if !ok || n == 0 {
		return
	}
	batch := make([]Event, 0, int(n))
	for _, e := range unsafe.Slice(events, int(n)) {
		ev := Event{ID: uint64(e.id), Op: toOp(uint32(e.flags)), Inode: uint64(e.inode)}
		if e.path != nil {
			ev.Path = C.GoString(e.path)
		}
		batch = append(batch, ev)
	}
	w.deliver(batch)
}
