//go:build linux && (arm64 || amd64) && cgo

package main

/*
#include "sandbox.h"
*/
import "C"

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	gcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

var libraryMu sync.Mutex

//export RunSandbox
func RunSandbox(guest *C.char, imageFD C.int, owner C.uintptr_t, callback C.inspect_fn, message *C.char, capacity C.size_t) (code C.int) {
	libraryMu.Lock()
	defer libraryMu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	log.SetLevel(log.Warning)
	report := func(err error) {
		code = 1
		if capacity > 0 {
			buf := unsafe.Slice((*byte)(unsafe.Pointer(message)), int(capacity))
			n := copy(buf[:len(buf)-1], err.Error())
			buf[n] = 0
		}
	}
	defer func() {
		if v := recover(); v != nil {
			report(fmt.Errorf("Sentry startup panicked: %v", v))
		}
	}()
	err := runSentry("/", C.GoString(guest), int(imageFD), func(_ gcontext.Context, _ platform.MemoryManager, ac *arch.Context64) {
		event := C.struct_syscall_event{number: C.uint64_t(ac.SyscallNo())}
		for i, arg := range ac.SyscallArgs() {
			event.args[i] = C.uint64_t(arg.Uint64())
		}
		C.invoke_inspector(callback, owner, &event)
		var args [6]uint64
		for i, arg := range event.args {
			args[i] = uint64(arg)
		}
		setSyscall(ac, uint64(event.number), args)
		ac.SyscallSaveOrig()
	})
	if err != nil {
		report(err)
	}
	return code
}

func main() {}
