//go:build linux && (arm64 || amd64) && cgo

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xgo-dev/sandbox"
	"golang.org/x/sys/unix"
)

func checkKernelLifecycle() error {
	// Count Kernel backing files, excluding Systrap's process-lifetime pools.
	countKernels := func() (int, error) {
		files, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return 0, err
		}
		n := 0
		for _, file := range files {
			target, _ := os.Readlink(filepath.Join("/proc/self/fd", file.Name()))
			if strings.Contains(target, "llar-runtime-memory") {
				n++
			}
		}
		return n, nil
	}
	if err := sandbox.Run(staticCall); err != nil {
		return err
	}
	before, err := countKernels()
	if err != nil {
		return err
	}
	if err := sandbox.Run(staticCall); err != nil {
		return err
	}
	after, err := countKernels()
	if err != nil || before != 1 || after != before {
		return fmt.Errorf("default Kernel reuse: before=%d after=%d error=%v", before, after, err)
	}

	const rendezvous = 0xffffffe0
	var arrived atomic.Int32
	release := make(chan struct{})
	s := sandbox.Sandbox{
		Env: []string{"SANDBOX_ENV_TEST=shared-kernel"},
		Mounts: []sandbox.Mount{
			{Type: "bind", Source: "/", Target: "/", Options: []string{"ro"}},
			{Type: "tmpfs", Target: "/tmp", Options: []string{"mode=1777"}},
			{Type: "proc", Target: "/proc"},
		},
		Inspect: func(call *sandbox.Syscall) {
			if call.Number != rendezvous {
				return
			}
			if arrived.Add(1) == 2 {
				close(release)
			}
			select {
			case <-release:
			case <-time.After(20 * time.Second):
				panic("guests did not overlap")
			}
			call.Number = unix.SYS_GETPID
		},
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(executable); strings.HasPrefix(dir, "/tmp/") {
		// CI keeps the executable under /tmp, which the private tmpfs hides.
		s.Mounts = append(s.Mounts, sandbox.Mount{Type: "bind", Source: dir, Target: dir, Options: []string{"ro"}})
	}
	defer s.Close()
	results := make(chan error, 2)
	for id := range 2 {
		go func() {
			value := id
			err := s.Run(func() {
				check(os.Getpid() == 1, "per-Run PID namespace")
				check(environmentAtInit == "shared-kernel", "per-Run environment")
				if err := os.WriteFile("/tmp/isolated", []byte{byte(value)}, 0600); err != nil {
					panic(err)
				}
				pid, _, errno := unix.RawSyscall(rendezvous, 0, 0, 0)
				check(errno == 0 && pid == 1, "concurrent inspector rewrite")
				data, err := os.ReadFile("/tmp/isolated")
				check(err == nil && len(data) == 1 && data[0] == byte(value), "independent tmpfs")
				proc, err := os.Readlink("/proc/self")
				check(err == nil && proc == "1", "per-Run procfs")
				value += 10
			})
			if err == nil && value != id+10 {
				err = fmt.Errorf("concurrent writeback: %d", value)
			}
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			return err
		}
	}
	if arrived.Load() != 2 {
		return fmt.Errorf("concurrent inspectors: %d", arrived.Load())
	}
	// Keep one guest inside its callback until the other Run has failed. A
	// Kernel-wide kill here would also terminate this surviving guest.
	const failure = 0xffffffe2
	survivorReady := make(chan struct{})
	failed := make(chan struct{})
	s.Inspect = func(call *sandbox.Syscall) {
		if call.Number != failure {
			return
		}
		if call.Args[0] == 0 {
			<-survivorReady
			if _, err := call.Malloc(8); err != nil {
				panic(err)
			}
			panic("expected isolated inspection failure")
		}
		close(survivorReady)
		<-failed
		call.Number = unix.SYS_GETPID
	}
	for id := range 2 {
		go func() {
			value := id
			err := s.Run(func() {
				unix.RawSyscall(failure, uintptr(value), 0, 0)
				value += 10
			})
			if id == 0 {
				close(failed)
				if err == nil || value != 0 || !strings.Contains(err.Error(), "expected isolated inspection failure") {
					results <- fmt.Errorf("inspection failure/writeback: value=%d error=%v", value, err)
					return
				}
				err = nil
			} else if err == nil && value != 11 {
				err = fmt.Errorf("surviving guest writeback: %d", value)
			}
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			return err
		}
	}
	if err := s.Run(staticCall); err != nil {
		return fmt.Errorf("reuse after concurrent guests: %w", err)
	}
	if err := s.Close(); err != nil {
		return err
	}
	if err := s.Run(staticCall); err == nil {
		return fmt.Errorf("Run after Close succeeded")
	}
	// MemoryFile.Destroy wakes a background releaser; Close may return first.
	deadline := time.Now().Add(20 * time.Second)
	for {
		after, err = countKernels()
		if err != nil {
			return fmt.Errorf("Kernel cleanup: %w", err)
		}
		if after == before {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Kernel cleanup timed out: before=%d after=%d", before, after)
		}
		time.Sleep(10 * time.Millisecond)
	}

	const waiting = 0xffffffe1
	ready := make(chan struct{})
	active := sandbox.Sandbox{Inspect: func(call *sandbox.Syscall) {
		if call.Number == waiting {
			close(ready)
			call.Number = unix.SYS_GETPID
		}
	}}
	defer active.Close()
	go func() {
		results <- active.Run(func() {
			unix.RawSyscall(waiting, 0, 0, 0)
			for {
				time.Sleep(time.Second)
			}
		})
	}()
	select {
	case <-ready:
	case err := <-results:
		return fmt.Errorf("active guest did not start: %v", err)
	case <-time.After(20 * time.Second):
		return fmt.Errorf("active guest startup timed out")
	}
	if err := active.Close(); err != nil {
		return err
	}
	if err := <-results; err == nil {
		return fmt.Errorf("terminated guest Run succeeded")
	}
	if err := sandbox.Run(staticCall); err != nil {
		return fmt.Errorf("closing another Sandbox stopped the default Kernel: %w", err)
	}
	fmt.Println("PASS default Kernel reuse, concurrent guests, isolated tmpfs/procfs, Close, and independent Kernels")
	return nil
}
