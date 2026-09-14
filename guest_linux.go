//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

const guestArgument = "--llar-sandbox-guest"

// Guest runs the imported closure when this process is Sentry's guest. Call it
// at the start of main, and exit on handled=true, including when err is nil.
// Do not call it from init: c-shared callbacks require completed Go init.
func Guest() (handled bool, err error) {
	if len(os.Args) != 2 || os.Args[1] != guestArgument {
		return false, nil
	}
	handled = true
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("sandbox guest panicked: %v", value)
		}
	}()
	m, err := loadMetadata()
	if err != nil {
		return true, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(3, &stat); err != nil {
		return true, err
	}
	if stat.Size != 2*imageBytes {
		return true, fmt.Errorf("invalid sandbox image size %d", stat.Size)
	}
	mem, err := unix.Mmap(3, 0, 2*imageBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return true, err
	}
	defer unix.Munmap(mem)
	defer unix.Close(3)
	functions := make(map[uintptr]nativeLayout)
	in := newImage(append([]byte(nil), mem[:imageBytes]...), m, functions)
	if err := in.header(); err != nil {
		return true, err
	}
	if err := in.authorizeFunctions(); err != nil {
		return true, err
	}
	fn, retained, err := in.decode(nil)
	if err != nil {
		return true, err
	}
	fn.Interface().(func())()
	runtime.GC()
	out := newImage(mem[imageBytes:], m, functions)
	if _, err := out.encode(fn, retained); err != nil {
		return true, err
	}
	binary.LittleEndian.PutUint64(mem[imageBytes+48:], 1)
	runtime.KeepAlive(retained)
	return true, nil
}
