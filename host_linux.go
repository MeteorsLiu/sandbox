//go:build linux && (arm64 || amd64) && cgo

package sandbox

/*
#cgo LDFLAGS: -ldl
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/cgo"
	"strings"
	"sync"
	"unsafe"

	"github.com/xgo-dev/sandbox/internal/state"
	"golang.org/x/sys/unix"
)

var runMu sync.Mutex

type inspection struct {
	mu  sync.Mutex
	fn  func(*Syscall)
	err error
}

//export sandboxInspect
func sandboxInspect(owner C.uintptr_t, event *C.struct_syscall_event) {
	i := cgo.Handle(owner).Value().(*inspection)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.fn == nil || i.err != nil {
		return
	}
	v := Syscall{Number: uint64(event.number), Name: C.GoString(event.name)}
	for n := range v.Args {
		v.Args[n] = uint64(event.args[n])
	}
	a := &syscallAccess{active: true}
	v.access = a
	mapped := false
	defer func() {
		if value := recover(); value != nil {
			i.err = fmt.Errorf("inspector panicked: %v", value)
			if mapped {
				event.failure = C.CString(i.err.Error())
			}
		}
		a.mu.Lock()
		a.active = false
		a.mmap = nil
		a.mu.Unlock()
	}()
	a.mmap = func(address uint64, size int) (Memory, error) {
		var memory C.struct_syscall_memory
		message := C.inspect_mmap(event, C.uint64_t(address), C.size_t(size), &memory)
		view := Memory{Data: unsafe.Slice((*byte)(memory.data), int(memory.length)), Addr: uint64(memory.address)}
		mapped = mapped || len(view.Data) != 0
		if message == nil {
			return view, nil
		}
		defer C.free(unsafe.Pointer(message))
		return view, fmt.Errorf("sandbox inspection: %s", C.GoString(message))
	}
	i.fn(&v)
	event.number = C.uint64_t(v.Number)
	for n, arg := range v.Args {
		event.args[n] = C.uint64_t(arg)
	}
}

// Run executes fn in a fresh guest running the same ELF. The guest enters the
// closure automatically after package initialization, without running main.
// Capture mutations are committed only after a successful guest exit.
// Captures must be exclusively owned for the duration of Run. Calls cannot
// overlap because the embedded Sentry runtime owns process-wide resources.
func (s *Sandbox) Run(fn func()) error {
	if fn == nil {
		return fmt.Errorf("sandbox: nil function")
	}
	if !runMu.TryLock() {
		return fmt.Errorf("sandbox: another Run is active")
	}
	defer runMu.Unlock()
	if !validRseqSetting(os.Getenv("GLIBC_TUNABLES")) {
		return fmt.Errorf("sandbox: start the process with GLIBC_TUNABLES=glibc.pthread.rseq=0")
	}
	mainPC, err := guestMain()
	if err != nil {
		return err
	}
	entryPC := reflect.ValueOf(guestEntry).Pointer()
	fd, err := newStateImage()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var graph state.State
	ctx := context.Background()
	resultOffset, err := writeStateImage(fd, 0, &graph, &fn)
	if err != nil {
		return fmt.Errorf("sandbox export: %w", err)
	}
	defer runtime.KeepAlive(&graph)
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	library := s.Library
	if library == "" {
		library = filepath.Join(filepath.Dir(executable), "sentrylib.so")
	}
	library, err = filepath.EvalSymlinks(library)
	if err != nil {
		return err
	}
	library, err = filepath.Abs(library)
	if err != nil {
		return err
	}
	mounts := s.Mounts
	if len(mounts) == 0 {
		mounts = []Mount{
			{Type: "bind", Source: "/", Target: "/", Options: []string{"ro"}},
			{Type: "proc", Target: "/proc"},
		}
	}
	env := s.Env
	if env == nil {
		env = os.Environ()
	}
	config, err := json.Marshal(struct {
		Guest  string   `json:"guest"`
		Mounts []Mount  `json:"mounts"`
		Env    []string `json:"env"`
	}{executable, mounts, env})
	if err != nil {
		return fmt.Errorf("sandbox configuration: %w", err)
	}
	cLibrary, cConfig := C.CString(library), C.CString(string(config))
	defer C.free(unsafe.Pointer(cLibrary))
	defer C.free(unsafe.Pointer(cConfig))
	i := &inspection{fn: s.Inspect}
	var handle cgo.Handle
	if s.Inspect != nil {
		handle = cgo.NewHandle(i)
		defer handle.Delete()
	}
	var message [4096]C.char
	code := C.sandbox_load(cLibrary, cConfig, C.int(fd), C.uintptr_t(mainPC), C.uintptr_t(entryPC), C.uintptr_t(handle), &message[0], C.size_t(len(message)))
	if code != 0 {
		return fmt.Errorf("sandbox Sentry: %s", C.GoString(&message[0]))
	}
	i.mu.Lock()
	inspectionErr := i.err
	i.mu.Unlock()
	if inspectionErr != nil {
		return inspectionErr
	}
	data, err := readStateImage(fd, resultOffset)
	if err != nil {
		return fmt.Errorf("sandbox result: %w", err)
	}
	if _, err := graph.Load(ctx, data, &fn); err != nil {
		return fmt.Errorf("sandbox import: %w", err)
	}
	return nil
}

func validRseqSetting(value string) bool {
	for _, setting := range strings.Split(value, ":") {
		if strings.HasPrefix(setting, "glibc.pthread.rseq=") {
			return setting == "glibc.pthread.rseq=0"
		}
	}
	return false
}
