//go:build linux && (arm64 || amd64) && cgo

package main

import (
	"fmt"
	"os"
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

func check(ok bool, message string) {
	if !ok {
		panic(message)
	}
}
func staticCall() { runtime.GC() }

func main() {
	if handled, err := sandbox.Guest(); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
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
	if err := s.Run(func() { n++; runtime.GC() }); err != nil {
		return fmt.Errorf("integer: %w", err)
	}
	check(n == 42 && os.Getpid() == pid, "integer writeback/host identity")
	fmt.Println("PASS integer capture, host PID, GC, and Switch inspector")

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
	return nil
}
