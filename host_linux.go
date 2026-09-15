//go:build linux && (arm64 || amd64) && cgo

package sandbox

/*
#cgo LDFLAGS: -ldl
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/cgo"
	"sync"
	"unsafe"

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
	defer func() {
		if value := recover(); value != nil {
			i.err = fmt.Errorf("inspector panicked: %v", value)
		}
	}()
	if i.fn == nil || i.err != nil {
		return
	}
	v := Syscall{Number: uint64(event.number), Name: C.GoString(event.name)}
	for n := range v.Args {
		v.Args[n] = uint64(event.args[n])
	}
	a := &syscallAccess{active: true}
	v.access = a
	defer func() {
		a.mu.Lock()
		a.active = false
		a.read = nil
		a.mu.Unlock()
	}()
	a.read = func(address uint64, dst []byte) (int, error) {
		var copied C.size_t
		message := C.inspect_read(event, C.uint64_t(address), unsafe.Pointer(unsafe.SliceData(dst)), C.size_t(len(dst)), &copied)
		if message == nil {
			return int(copied), nil
		}
		defer C.free(unsafe.Pointer(message))
		return int(copied), fmt.Errorf("sandbox inspection: %s", C.GoString(message))
	}
	i.fn(&v)
	event.number = C.uint64_t(v.Number)
	for n, arg := range v.Args {
		event.args[n] = C.uint64_t(arg)
	}
}

func headerWord(mem []byte, offset int) uintptr {
	return uintptr(binary.LittleEndian.Uint64(mem[offset : offset+8]))
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
	m, err := loadMetadata()
	if err != nil {
		return err
	}
	jumpSize := uint64(4)
	if runtime.GOARCH == "amd64" {
		jumpSize = 5
	}
	if m.mainPC == 0 || m.mainSize < jumpSize {
		return fmt.Errorf("sandbox needs a main.main ELF symbol with at least %d bytes", jumpSize)
	}
	entryPC := reflect.ValueOf(guestEntry).Pointer()
	fd, err := unix.MemfdCreate("llar-sandbox", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, 2*imageBytes); err != nil {
		return err
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		return err
	}
	mem, err := unix.Mmap(fd, 0, 2*imageBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return err
	}
	defer unix.Munmap(mem)
	functions := make(map[uintptr]nativeLayout)
	in := newImage(mem[:imageBytes], m, functions)
	w, err := in.encode(reflect.ValueOf(fn), nil)
	if err != nil {
		return fmt.Errorf("sandbox export: %w", err)
	}
	defer runtime.KeepAlive(w)
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
	cLibrary, cExecutable := C.CString(library), C.CString(executable)
	defer C.free(unsafe.Pointer(cLibrary))
	defer C.free(unsafe.Pointer(cExecutable))
	i := &inspection{fn: s.Inspect}
	var handle cgo.Handle
	if s.Inspect != nil {
		handle = cgo.NewHandle(i)
		defer handle.Delete()
	}
	var message [4096]C.char
	code := C.sandbox_load(cLibrary, cExecutable, C.int(fd), C.uintptr_t(m.mainPC), C.uintptr_t(entryPC), C.uintptr_t(handle), &message[0], C.size_t(len(message)))
	if code != 0 {
		return fmt.Errorf("sandbox Sentry: %s", C.GoString(&message[0]))
	}
	i.mu.Lock()
	inspectionErr := i.err
	i.mu.Unlock()
	if inspectionErr != nil {
		return inspectionErr
	}
	// Never parse guest-writable memory while changing host objects.
	output := append([]byte(nil), mem[imageBytes:]...)
	if headerWord(output, 48) != 1 {
		return fmt.Errorf("sandbox guest did not publish a completed result")
	}
	out := newImage(output, m, functions)
	if err := out.commit(w.anchors); err != nil {
		return fmt.Errorf("sandbox import: %w", err)
	}
	return nil
}
