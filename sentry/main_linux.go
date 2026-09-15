//go:build linux && (arm64 || amd64) && cgo

package main

/*
#include <stdlib.h>
#include "sandbox.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"runtime/cgo"
	"sync"
	"unsafe"

	"gvisor.dev/gvisor/pkg/abi/linux"
	gcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
)

var libraryMu sync.Mutex

//export RunSandbox
func RunSandbox(config *C.char, imageFD C.int, mainPC, entryPC, owner C.uintptr_t, callback C.inspect_fn, message *C.char, capacity C.size_t) (code C.int) {
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
	var startup struct {
		Guest  string  `json:"guest"`
		Mounts []mount `json:"mounts"`
	}
	if err := json.Unmarshal([]byte(C.GoString(config)), &startup); err != nil {
		report(fmt.Errorf("Sentry startup configuration: %w", err))
		return code
	}
	installSyscallMemory()
	var inspectionMu sync.Mutex
	var inspectionErr error
	err := runSentry(startup.Mounts, startup.Guest, int(imageFD), uintptr(mainPC), uintptr(entryPC), func(ctx gcontext.Context, ac *arch.Context64) error {
		if callback == nil {
			return nil
		}
		task := kernel.TaskFromContext(ctx)
		m := &syscallMemory{ctx: task.Kernel().SupervisorContext(), mm: task.MemoryManager(), task: task}
		handle := cgo.NewHandle(m)
		defer handle.Delete()
		event := C.struct_syscall_event{number: C.uint64_t(ac.SyscallNo())}
		name := C.CString(task.SyscallTable().LookupName(ac.SyscallNo()))
		defer C.free(unsafe.Pointer(name))
		event.name = name
		C.prepare_inspection(&event, C.uintptr_t(handle))
		for i, arg := range ac.SyscallArgs() {
			event.args[i] = C.uint64_t(arg.Uint64())
		}
		C.invoke_inspector(callback, owner, &event)
		var err error
		if event.failure != nil {
			err = fmt.Errorf("syscall inspection: %s", C.GoString(event.failure))
			C.free(unsafe.Pointer(event.failure))
		}
		if len(m.regions) != 0 {
			if err != nil {
				err = errors.Join(err, m.release())
			} else {
				memoryCalls.Lock()
				memoryCalls.pending[task] = m
				memoryCalls.live[m] = struct{}{}
				memoryCalls.Unlock()
			}
		}
		if err != nil {
			inspectionMu.Lock()
			if inspectionErr == nil {
				inspectionErr = err
			}
			inspectionMu.Unlock()
			task.Kernel().Kill(linux.WaitStatusExit(1))
			return err
		}
		var args [6]uint64
		for i, arg := range event.args {
			args[i] = uint64(arg)
		}
		setSyscall(ac, uint64(event.number), args)
		ac.SyscallSaveOrig()
		return nil
	})
	if inspectionErr != nil {
		err = inspectionErr
	}
	if err != nil {
		report(err)
	}
	return code
}

func main() {}
