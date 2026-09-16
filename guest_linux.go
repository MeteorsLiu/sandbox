//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"

	"github.com/xgo-dev/sandbox/internal/state"
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
	data, err := stateImage(append([]byte(nil), mem[:imageBytes]...))
	if err != nil {
		return err
	}
	var graph state.State
	var fn func()
	ctx := context.Background()
	if _, err := graph.Load(ctx, data, &fn); err != nil {
		return err
	}
	fn()
	n, _, err := graph.Save(ctx, mem[imageBytes+8:], &fn)
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint64(mem[imageBytes:], uint64(n))
	runtime.KeepAlive(&graph)
	return nil
}
