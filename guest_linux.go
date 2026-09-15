//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// guestEntry replaces main.main in the guest after Go package initialization.
// Its address is passed by Run, keeping this entry and its dependencies linked.
//
//go:noinline
func guestEntry() {
	if err := runGuest(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runGuest() (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("sandbox guest panicked: %v", value)
		}
	}()
	m, err := loadMetadata()
	if err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(3, &stat); err != nil {
		return err
	}
	if stat.Size != 2*imageBytes {
		return fmt.Errorf("invalid sandbox image size %d", stat.Size)
	}
	mem, err := unix.Mmap(3, 0, 2*imageBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return err
	}
	defer unix.Munmap(mem)
	defer unix.Close(3)
	functions := make(map[uintptr]nativeLayout)
	in := newImage(append([]byte(nil), mem[:imageBytes]...), m, functions)
	if err := in.prepare(nil); err != nil {
		return err
	}
	bindings, err := in.globalBindings()
	if err != nil {
		return err
	}
	fn, retained, err := in.decode(bindings)
	if err != nil {
		return err
	}
	fn.Interface().(func())()
	out := newImage(mem[imageBytes:], m, functions)
	out.inherit(in)
	if _, err := out.encode(fn, retained); err != nil {
		return err
	}
	binary.LittleEndian.PutUint64(mem[imageBytes+48:], 1)
	runtime.KeepAlive(retained)
	return nil
}
