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

	"gvisor.dev/gvisor/pkg/hostarch"
)

//export MMapSyscallMemory
func MMapSyscallMemory(owner C.uintptr_t, address C.uint64_t, size C.size_t, memory *C.struct_syscall_memory) *C.char {
	m := cgo.Handle(owner).Value().(*syscallMemory)
	data, addr, err := m.mmap(hostarch.Addr(address), uint64(size))
	memory.data = unsafe.Pointer(unsafe.SliceData(data))
	memory.address = C.uint64_t(addr)
	memory.length = C.size_t(len(data))
	if err != nil {
		return C.CString(err.Error())
	}
	return nil
}
