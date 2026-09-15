//go:build linux && (amd64 || arm64) && cgo

package main

/*
#include <stdlib.h>
#include "sandbox.h"
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/usermem"
)

type sentryMemory struct {
	ctx context.Context
	mm  *mm.MemoryManager
}

//export ReadSyscallMemory
func ReadSyscallMemory(owner C.uintptr_t, address C.uint64_t, dst unsafe.Pointer, size C.size_t, copied *C.size_t) *C.char {
	m := cgo.Handle(owner).Value().(sentryMemory)
	n, err := m.mm.CopyIn(m.ctx, hostarch.Addr(address), unsafe.Slice((*byte)(dst), int(size)), usermem.IOOpts{})
	*copied = C.size_t(n)
	if err != nil {
		return C.CString(err.Error())
	}
	return nil
}
