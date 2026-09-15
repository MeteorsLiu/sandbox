//go:build linux && (amd64 || arm64) && cgo

package main

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

func TestTemporaryMemoryAliases(t *testing.T) {
	if err := usage.Init(); err != nil {
		t.Fatal(err)
	}
	fd, err := memutil.CreateMemFD("temporary-memory-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "temporary-memory-test")
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	defer mf.Destroy()
	fr, err := mf.Allocate(3*hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateAndCommit})
	if err != nil {
		t.Fatal(err)
	}
	defer mf.DecRef(fr)
	blocks, err := mf.MapInternal(fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if blocks.NumBlocks() != 1 {
		t.Fatal("fresh three-page allocation crossed a MemoryFile chunk")
	}
	original := blocks.Head().ToSlice()
	original[0], original[2*hostarch.PageSize] = 'a', 'b'
	pins := []mm.PinnedRange{{
		Source: hostarch.AddrRange{Start: hostarch.PageSize, End: 4 * hostarch.PageSize},
		File:   mf, Offset: fr.Start,
	}}
	view, owned, err := mapTemporaryMemory(pins, len(original))
	if err != nil || owned || len(view) != len(original) {
		t.Fatalf("direct alias: length=%d, owned=%v, error=%v", len(view), owned, err)
	}
	view[0] = 'c'
	if original[0] != 'c' {
		t.Fatal("direct view copied the backing pages")
	}

	// Reverse two non-adjacent backing pages in a contiguous guest range.
	pins = []mm.PinnedRange{
		{Source: hostarch.AddrRange{Start: hostarch.PageSize, End: 2 * hostarch.PageSize}, File: mf, Offset: fr.Start + 2*hostarch.PageSize},
		{Source: hostarch.AddrRange{Start: 2 * hostarch.PageSize, End: 3 * hostarch.PageSize}, File: mf, Offset: fr.Start},
	}
	view, owned, err = mapTemporaryMemory(pins, 2*hostarch.PageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unix.Munmap(view); err != nil {
			t.Error(err)
		}
	}()
	if !owned || len(view) != 2*hostarch.PageSize || view[0] != 'b' || view[hostarch.PageSize] != 'c' {
		t.Fatal("fragmented view mapped the wrong file ranges")
	}
	view[0], view[hostarch.PageSize] = 'x', 'y'
	if original[2*hostarch.PageSize] != 'x' || original[0] != 'y' {
		t.Fatal("fragmented view copied the backing pages")
	}
	original[0] = 'z'
	if view[hostarch.PageSize] != 'z' {
		t.Fatal("backing edits were not visible through the host alias")
	}
}
