//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"context"
	"errors"
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
	defer unix.Close(3)
	data, unmap, err := readStateImage(3, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unmap()) }()
	var graph state.State
	var fn func()
	ctx := context.Background()
	if _, err := graph.Load(ctx, data, &fn); err != nil {
		return err
	}
	runtime.GC()
	fn()
	_, err = writeStateImage(3, int64(len(data))+8, &graph, &fn)
	if err != nil {
		return err
	}
	runtime.GC()
	runtime.KeepAlive(&graph)
	return nil
}
