//go:build linux && (amd64 || arm64) && cgo

package ixgo_test

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"testing"

	"github.com/goplus/ixgo"
	_ "github.com/goplus/ixgo/pkg/sync/atomic"
	"github.com/xgo-dev/sandbox/internal/state"
)

func atomicPointerClosure(t *testing.T) func() int {
	t.Helper()
	const path = "testdata/atomic_pointer.go"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	interp, err := ixgo.NewContext(ixgo.SupportMultipleInterp).LoadInterp(path, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interp.UnsafeRelease)
	if err := interp.RunInit(); err != nil {
		t.Fatal(err)
	}
	value, err := interp.RunFunc("Prepare")
	if err != nil {
		t.Fatal(err)
	}
	return value.(func() int)
}

func TestIxgoAtomicPointerState(t *testing.T) {
	original := atomicPointerClosure(t)
	if got := original(); got != 1 {
		t.Fatalf("host before Save: got %d, want 1", got)
	}
	ctx := context.Background()
	save := func(graph *state.State, fn *func() int) []byte {
		data := make([]byte, 1<<20)
		for {
			n, _, err := graph.Save(ctx, data, fn)
			if err == nil {
				return data[:n]
			}
			if !errors.Is(err, io.ErrShortBuffer) {
				t.Fatalf("Save: %v", err)
			}
			data = make([]byte, 2*len(data))
		}
	}
	var host, guest state.State
	src := original
	data := save(&host, &src)
	var dst func() int
	if _, err := guest.Load(ctx, data, &dst); err != nil {
		t.Fatalf("guest Load: %v", err)
	}
	runtime.GC()
	for _, want := range []int{2, 3} {
		if got := dst(); got != want {
			t.Fatalf("guest: got %d, want %d", got, want)
		}
	}
	data = save(&guest, &dst)
	if _, err := host.Load(ctx, data, &src); err != nil {
		t.Fatalf("host Load: %v", err)
	}
	runtime.GC()
	if got := original(); got != 4 {
		t.Fatalf("original closure after writeback: got %d, want 4", got)
	}
}

func TestIxgoAtomicPointerSandbox(t *testing.T) {
	fn := atomicPointerClosure(t)
	if got := fn(); got != 1 {
		t.Fatalf("host before Run: got %d, want 1", got)
	}
	var first, second int
	if err := runSandbox(t, func() {
		runtime.GC()
		first, second = fn(), fn()
	}); err != nil {
		t.Fatal(err)
	}
	if first != 2 || second != 3 {
		t.Fatalf("guest: got %d, %d, want 2, 3", first, second)
	}
	runtime.GC()
	if got := fn(); got != 4 {
		t.Fatalf("original closure after Run: got %d, want 4", got)
	}
}
