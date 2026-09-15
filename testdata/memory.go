//go:build linux && (arm64 || amd64) && cgo

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/xgo-dev/sandbox"
	"golang.org/x/sys/unix"
)

func inspectTemporaryMemory() error {
	dir, err := os.MkdirTemp("", "sandbox-memory-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0755); err != nil {
		return err
	}
	original := filepath.Join(dir, "original")
	replacement := filepath.Join(dir, "replaced")
	if err := os.WriteFile(original, []byte("original file"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(replacement, []byte("replacement file"), 0644); err != nil {
		return err
	}
	var retained *sandbox.Syscall
	var cleanup [3]uint64
	paths, writes, largeWrites, cleaned := 0, 0, 0, 0
	boundaryReads := 0
	s := sandbox.Sandbox{Inspect: func(call *sandbox.Syscall) {
		switch call.Name {
		case "openat":
			args := call.Args
			view, err := call.MMap(args[1], len(original)+1)
			if err != nil || string(view.Data) != original+"\x00" {
				return
			}
			copy(view.Data, replacement+"\x00")
			before, err := call.MMap(args[1], len(original)+1)
			check(err == nil && string(before.Data) == original+"\x00", "original guest pathname changed")
			after, err := call.MMap(view.Addr, len(replacement)+1)
			check(err == nil && string(after.Data) == replacement+"\x00", "temporary pages did not reflect host edits immediately")
			check(call.Args == args, "framework rewrote syscall arguments")
			call.Args[1] = view.Addr
			retained = call
			paths++
		case "writev":
			if call.Args[2] != 1 {
				return
			}
			args := call.Args
			vector, err := call.MMap(args[1], 16)
			if err != nil {
				panic(err)
			}
			address := binary.NativeEndian.Uint64(vector.Data[:8])
			size := int(binary.NativeEndian.Uint64(vector.Data[8:]))
			if size != len("initial input") && size != unix.Getpagesize()+1 {
				return
			}
			payload, err := call.MMap(address, size)
			if err != nil {
				panic(err)
			}
			if size == len("initial input") {
				check(string(payload.Data) == "initial input", "writev input mismatch")
				copy(payload.Data, "edited output")
				writes++
			} else {
				check(payload.Data[0] == 'x' && payload.Data[size-1] == 'x', "cross-page input mismatch")
				payload.Data[0], payload.Data[size-1] = 'a', 'z'
				largeWrites++
			}
			check(call.Args == args && binary.NativeEndian.Uint64(vector.Data[:8]) == address, "framework guessed a nested pointer")
			// Both indirections are the interceptor's responsibility.
			binary.NativeEndian.PutUint64(vector.Data[:8], payload.Addr)
			call.Args[1] = vector.Addr
			visible, err := call.MMap(payload.Addr, size)
			check(err == nil && string(visible.Data) == string(payload.Data), "nested payload required a commit")
			cleanup = [3]uint64{vector.Addr, payload.Addr, visible.Addr}
		case "mincore":
			if call.Args[0] == 0 && cleaned < len(cleanup) {
				check(cleanup[cleaned] != 0, "missing cleanup address")
				call.Args[0] = cleanup[cleaned]
				cleaned++
			}
		case "getpid":
			switch call.Args[0] {
			case 0x11223344:
				view, err := call.MMap(call.Args[1], unix.Getpagesize())
				check(err != nil && len(view.Data) == 0, "unmapped source became readable during temporary allocation")
				boundaryReads++
			case 0x11223345:
				view, err := call.MMap(call.Args[1], 16)
				check(err != nil && string(view.Data) == "prefix!!" && view.Addr != 0, "partial read lost its valid prefix")
				boundaryReads++
			case 0x11223346:
				view, err := call.MMap(call.Args[1], 8)
				check(err != nil && len(view.Data) == 0, "read ignored guest memory permissions")
				boundaryReads++
			}
		}
	}}
	var content string
	if err := s.Run(func() {
		data, err := os.ReadFile(original)
		if err != nil {
			panic(err)
		}
		content = string(data)
		var group sync.WaitGroup
		for range 8 {
			group.Go(func() {
				writeTemporaryInput([]byte("initial input"), "edited output")
			})
		}
		group.Wait()
		large := strings.Repeat("x", unix.Getpagesize()+1)
		writeTemporaryInput([]byte(large), "a"+large[1:len(large)-1]+"z")
		// Probe guest mappings without MMap, which itself allocates pages.
		for range 3 {
			var residency byte
			_, _, errno := unix.RawSyscall(unix.SYS_MINCORE, 0, uintptr(unix.Getpagesize()), uintptr(unsafe.Pointer(&residency)))
			check(errno == unix.ENOMEM, "temporary guest mapping survived syscall completion")
		}
		dead, err := unix.Mmap(-1, 0, unix.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
		if err != nil {
			panic(err)
		}
		address := uintptr(unsafe.Pointer(unsafe.SliceData(dead)))
		if err := unix.Munmap(dead); err != nil {
			panic(err)
		}
		unix.RawSyscall(unix.SYS_GETPID, 0x11223344, address, 0)
		guarded, err := unix.Mmap(-1, 0, 2*unix.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
		if err != nil {
			panic(err)
		}
		defer unix.Munmap(guarded)
		copy(guarded[unix.Getpagesize()-8:], "prefix!!")
		if err := unix.Mprotect(guarded[unix.Getpagesize():], unix.PROT_NONE); err != nil {
			panic(err)
		}
		address = uintptr(unsafe.Pointer(unsafe.SliceData(guarded))) + uintptr(unix.Getpagesize())
		unix.RawSyscall(unix.SYS_GETPID, 0x11223345, address-8, 0)
		unix.RawSyscall(unix.SYS_GETPID, 0x11223346, address, 0)
		runtime.KeepAlive(guarded)
	}); err != nil {
		return fmt.Errorf("temporary syscall memory: %w", err)
	}
	check(paths == 1 && writes == 8 && largeWrites == 1 && cleaned == 3 && boundaryReads == 3 && content == "replacement file", "temporary path and vector replacements")
	if _, err := retained.MMap(1, 1); err == nil {
		return fmt.Errorf("expired memory access remained active")
	}

	n := 0
	failing := sandbox.Sandbox{Inspect: func(call *sandbox.Syscall) {
		if call.Name == "write" && call.Args[2] == uint64(len("panic payload")) {
			if _, err := call.MMap(call.Args[1], len("panic payload")); err != nil {
				panic(err)
			}
			panic("mapped inspection failure")
		}
	}}
	err = failing.Run(func() {
		unix.Write(-1, []byte("panic payload"))
		n = 1
	})
	if err == nil || !strings.Contains(err.Error(), "mapped inspection failure") || n != 0 {
		return fmt.Errorf("mapped inspector panic: %v, result=%d", err, n)
	}
	fmt.Println("PASS direct temporary-page edits, original memory unchanged, explicit path/nested pointer edits, cross-page and concurrent calls, cleanup, read boundaries and inspector panic")
	return nil
}

func writeTemporaryInput(input []byte, expected string) {
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		panic(err)
	}
	defer unix.Close(pipe[0])
	defer unix.Close(pipe[1])
	original := string(input)
	n, err := unix.Writev(pipe[1], [][]byte{input})
	check(err == nil && n == len(expected), "rewritten writev result")
	check(string(input) == original, "original guest payload changed")
	output := make([]byte, len(expected))
	n, err = unix.Read(pipe[0], output)
	check(err == nil && string(output[:n]) == expected, "writev nested pointer edit")
	runtime.KeepAlive(input)
}
