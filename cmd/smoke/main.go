//go:build linux && (arm64 || amd64) && cgo

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/xgo-dev/sandbox"
	"golang.org/x/sys/unix"
)

type node struct {
	Value int
	Next  *node
}

var initializedPID = os.Getpid()
var enteredMain bool

func check(ok bool, message string) {
	if !ok {
		panic(message)
	}
}
func staticCall() { runtime.GC() }

func main() {
	enteredMain = true
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var calls atomic.Int64
	s := sandbox.Sandbox{Inspect: func(call *sandbox.Syscall) { calls.Add(1) }}
	n := 41
	pid := os.Getpid()
	if err := s.Run(func() {
		check(!enteredMain && initializedPID == os.Getpid(), "guest must run package init and skip main")
		n++
		runtime.GC()
	}); err != nil {
		return fmt.Errorf("integer: %w", err)
	}
	check(n == 42 && os.Getpid() == pid, "integer writeback/host identity")
	fmt.Println("PASS automatic guest entry after init, integer capture, host PID, GC, and Switch inspector")

	a := &node{Value: 1}
	a.Next = a
	alias := a
	m := map[string]*node{"a": a}
	mapAlias := m
	backing := []int{1, 2, 3}
	values := backing[:2]
	var boxed any = a
	var result complex128
	inc := func() { a.Value++ }
	if err := s.Run(func() {
		check(a == m["a"] && a.Next == a && boxed.(*node) == a, "guest aliases/cycle")
		inc()
		values[0] = 9
		values = append(values, 8, 7)
		delete(m, "a")
		m["b"] = a
		result = complex(3, 4)
		boxed = "finished"
	}); err != nil {
		return fmt.Errorf("objects: %w", err)
	}
	check(a == alias && a.Next == a && a.Value == 2, "host pointer/cycle")
	check(mapAlias["b"] == a && len(mapAlias) == 1, "host map identity")
	check(backing[0] == 9 && len(values) == 4 && values[3] == 7, "slice writeback/growth")
	check(boxed == "finished" && result == complex(3, 4), "interface/scalars")
	fmt.Println("PASS pointers, cycle, map aliases, slice growth, interface, nested closure")

	p := &node{Value: 10}
	old := p
	if err := s.Run(func() { p.Value = 11; p = nil }); err != nil {
		return fmt.Errorf("dropped reference: %w", err)
	}
	check(p == nil && old.Value == 11, "dropped object mutation lost")
	fmt.Println("PASS mutation before dropping last captured reference")

	before := n
	if err := s.Run(func() { n = 999; panic("expected smoke panic") }); err == nil {
		return fmt.Errorf("guest panic was not reported")
	}
	check(n == before, "guest panic committed a partial result")
	fmt.Println("PASS guest panic leaves host captures unchanged")
	if err := s.Run(staticCall); err != nil {
		return fmt.Errorf("static function: %w", err)
	}
	fmt.Println("PASS static function and repeated Sentry startup")
	for range 3 {
		if err := s.Run(staticCall); err != nil {
			return err
		}
	}
	fdBefore, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	for range 3 {
		if err := s.Run(staticCall); err != nil {
			return err
		}
	}
	fdAfter, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	check(len(fdAfter) == len(fdBefore), fmt.Sprintf("per-call FD growth: %d -> %d", len(fdBefore), len(fdAfter)))
	fmt.Printf("PASS repeated-call descriptor count=%d\n", len(fdAfter))
	var guestPID uintptr
	var nestedErr error
	rewriter := sandbox.Sandbox{Inspect: func(call *sandbox.Syscall) {
		if call.Number == 0xfffffff0 {
			nestedErr = sandbox.Run(staticCall)
			call.Number = unix.SYS_GETPID
		}
	}}
	if err := rewriter.Run(func() {
		result, _, errno := unix.RawSyscall(0xfffffff0, 0, 0, 0)
		if errno != 0 {
			panic(errno)
		}
		guestPID = result
	}); err != nil {
		return fmt.Errorf("syscall rewrite: %w", err)
	}
	check(guestPID == 1 && nestedErr != nil, "syscall rewrite/nested Run guard")
	fmt.Println("PASS syscall number rewrite and nested Run rejection")

	var mu sync.Mutex
	if err := s.Run(func() { mu.Lock(); mu.Unlock() }); err == nil {
		return fmt.Errorf("sync.Mutex was accepted")
	}
	ch := make(chan int)
	if err := s.Run(func() { close(ch) }); err == nil {
		return fmt.Errorf("channel was accepted")
	}
	fmt.Println("PASS unsupported synchronization values rejected")

	var stdinBefore, stdinAfter unix.Stat_t
	if err := unix.Fstat(0, &stdinBefore); err != nil {
		return err
	}
	var text string
	if err := s.Run(func() {
		b, err := os.ReadFile("/etc/hostname")
		if err != nil {
			panic(err)
		}
		text = string(b)
	}); err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	if err := unix.Fstat(0, &stdinAfter); err != nil {
		return err
	}
	check(stdinBefore.Ino == stdinAfter.Ino && stdinBefore.Dev == stdinAfter.Dev && text != "", "stdin changed/read missing")
	check(calls.Load() > 0, "no inspector callbacks")
	fmt.Printf("PASS read syscall, stdin preserved, inspector callbacks=%d\n", calls.Load())
	if err := inspectMemory(); err != nil {
		return err
	}
	return nil
}

func inspectMemory() error {
	dir, err := os.MkdirTemp("", "sandbox-inspection-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0755); err != nil {
		return err
	}
	requested := filepath.Join(dir, "missing")
	actual := filepath.Join(dir, "longer-replacement-file")
	if err := os.WriteFile(actual, []byte("redirected file content"), 0644); err != nil {
		return err
	}
	var retained *sandbox.Syscall
	var snapshot *sandbox.DecodedSyscall
	var inspectionErr error
	pathChanged, bufferChanged := false, false
	s := sandbox.Sandbox{Inspect: func(call *sandbox.Syscall) {
		if inspectionErr != nil {
			return
		}
		switch call.Name {
		case "openat":
			decoded, err := call.Decode(8192)
			if err != nil {
				inspectionErr = err
				return
			}
			if decoded.Args[1].Value != requested {
				return
			}
			buf := make([]byte, len(requested)+1)
			n, err := call.ReadMemory(call.Args[1], buf)
			if err != nil || n != len(buf) || string(buf) != requested+"\x00" {
				inspectionErr = fmt.Errorf("guest memory read: %d %v", n, err)
				return
			}
			if _, err := call.ReadMemory(1, buf); err == nil {
				inspectionErr = fmt.Errorf("unmapped guest memory read succeeded")
				return
			}
			decoded.Args[1].Value = actual
			inspectionErr = call.Rewrite(decoded)
			pathChanged = inspectionErr == nil
			retained, snapshot = call, decoded
		case "write":
			if call.Args[2] != uint64(len("before-interceptor")) {
				return
			}
			decoded, err := call.Decode(1024)
			if err != nil {
				inspectionErr = err
				return
			}
			decoded.Args[1].Value = map[string]any{"bytes": []byte("after-interceptor-longer")}
			inspectionErr = call.Rewrite(decoded)
			bufferChanged = inspectionErr == nil
		}
	}}
	var content, received string
	var written int
	err = s.Run(func() {
		data, err := os.ReadFile(requested)
		if err != nil {
			panic(err)
		}
		content = string(data)
		var pipe [2]int
		if err := unix.Pipe(pipe[:]); err != nil {
			panic(err)
		}
		defer unix.Close(pipe[0])
		defer unix.Close(pipe[1])
		written, err = unix.Write(pipe[1], []byte("before-interceptor"))
		if err != nil {
			panic(err)
		}
		var buf [128]byte
		n, err := unix.Read(pipe[0], buf[:])
		if err != nil {
			panic(err)
		}
		received = string(buf[:n])
	})
	if inspectionErr != nil {
		return fmt.Errorf("structured inspector: %w", inspectionErr)
	}
	if err != nil {
		return fmt.Errorf("guest memory inspection: %w", err)
	}
	check(pathChanged && content == "redirected file content", "openat pathname rewrite")
	check(bufferChanged && received == "after-interceptor-longer" && written == len(received), "write buffer and count rewrite")
	if _, err := retained.ReadMemory(1, make([]byte, 1)); err == nil {
		return fmt.Errorf("retained ReadMemory was accepted")
	}
	if _, err := retained.Decode(1024); err == nil {
		return fmt.Errorf("retained Decode was accepted")
	}
	if err := retained.Rewrite(snapshot); err == nil {
		return fmt.Errorf("retained Rewrite was accepted")
	}
	fmt.Println("PASS lazy structured decode, guest memory reads, openat path and write buffer/count rewrites, callback lifetime")
	return nil
}
