//go:build linux && (amd64 || arm64) && cgo

package main

/*
#include <stdlib.h>
#include "sandbox.h"
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"runtime/cgo"
	"unsafe"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/usermem"
)

type sentryMemory struct {
	ctx context.Context
	mm  *mm.MemoryManager
}

func (m sentryMemory) read(address uint64, dst []byte) (int, error) {
	return m.mm.CopyIn(m.ctx, hostarch.Addr(address), dst, usermem.IOOpts{})
}

func (m sentryMemory) allocate(data []byte, fixups []pointerFixup) (uint64, error) {
	length := (uint64(len(data)) + hostarch.PageSize - 1) & ^uint64(hostarch.PageSize-1)
	address, err := m.mm.MMap(m.ctx, memmap.MMapOpts{
		Length: length, Private: true, Perms: hostarch.Read, MaxPerms: hostarch.ReadWrite,
		Name: "sandbox-syscall-input",
	})
	if err != nil {
		return 0, err
	}
	for _, fixup := range fixups {
		binary.LittleEndian.PutUint64(data[fixup.at:fixup.at+8], uint64(address)+uint64(fixup.target))
	}
	// CopyOut with IgnorePermissions still requires writable MaxPerms. The
	// initial guest protection is read-only, but guest mprotect can change it.
	// Keep the mapping across signal delivery and syscall restart.
	if _, err := m.mm.CopyOut(m.ctx, address, data, usermem.IOOpts{IgnorePermissions: true}); err != nil {
		_ = m.mm.MUnmap(m.ctx, address, length)
		return 0, err
	}
	return uint64(address), nil
}

func eventRegisters(event *C.struct_syscall_event) [6]uint64 {
	var args [6]uint64
	for n, arg := range event.args {
		args[n] = uint64(arg)
	}
	return args
}

//export ReadSyscallMemory
func ReadSyscallMemory(owner C.uintptr_t, address C.uint64_t, dst unsafe.Pointer, size C.size_t, copied *C.size_t) *C.char {
	c := cgo.Handle(owner).Value().(*syscallCodec)
	n, err := c.memory.read(uint64(address), unsafe.Slice((*byte)(dst), int(size)))
	*copied = C.size_t(n)
	if err != nil {
		return C.CString(err.Error())
	}
	return nil
}

//export DecodeSyscall
func DecodeSyscall(owner C.uintptr_t, event *C.struct_syscall_event, budget C.size_t, message **C.char) *C.char {
	c := cgo.Handle(owner).Value().(*syscallCodec)
	data, err := c.decode(uint64(event.number), eventRegisters(event), int(budget))
	if err != nil {
		*message = C.CString(err.Error())
		return nil
	}
	return C.CString(string(data))
}

//export RewriteSyscall
func RewriteSyscall(owner C.uintptr_t, event *C.struct_syscall_event, data unsafe.Pointer, size C.size_t) *C.char {
	c := cgo.Handle(owner).Value().(*syscallCodec)
	args, err := c.rewrite(uint64(event.number), eventRegisters(event), unsafe.Slice((*byte)(data), int(size)))
	if err != nil {
		return C.CString(fmt.Sprintf("rewrite: %v", err))
	}
	for n, arg := range args {
		event.args[n] = C.uint64_t(arg)
	}
	return nil
}
