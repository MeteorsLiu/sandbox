//go:build linux && (arm64 || amd64) && cgo

package main

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/usermem"
)

type sentryKernel struct {
	mu       sync.Mutex // Serializes process creation/start against Close.
	kernel   *kernel.Kernel
	platform *observedPlatform
	dog      *watchdog.Watchdog
	runs     sync.WaitGroup
	closed   bool
}

type guestProcess struct {
	inspect inspector
	tasks   sync.WaitGroup
}

// Kernel supervisor contexts fall back to the first task's mount namespace.
// A new Run has no mount namespace until its own root has been mounted.
type mountContext struct{ context.Context }

func (c mountContext) Value(key any) any {
	if key == vfs.CtxMountNamespace {
		return nil
	}
	return c.Context.Value(key)
}

func (s *sentryKernel) run(mounts []mount, executable string, env []string, imageFD int, mainPC, entryPC uintptr, inspect inspector) (err error) {
	if !path.IsAbs(executable) || strings.ContainsRune(executable, 0) {
		return errors.New("guest executable must be an absolute path without NUL")
	}
	for _, entry := range env {
		if strings.IndexByte(entry, 0) >= 0 {
			return errors.New("guest environment contains NUL")
		}
	}
	jump, err := entryJump(mainPC, entryPC)
	if err != nil {
		return err
	}
	defer closeMounts(mounts)
	if err := prepareMounts(mounts); err != nil {
		return err
	}

	k := s.kernel
	pidns := k.RootPIDNamespace().NewChild(k.SupervisorContext(), k, k.RootUserNamespace())
	defer pidns.DecRef(k.SupervisorContext())
	id := strconv.FormatUint(pidns.ID(), 10)
	process := &guestProcess{inspect: inspect}
	s.platform.mu.Lock()
	s.platform.processes[id] = process
	s.platform.mu.Unlock()
	defer func() {
		// Platform contexts are released after their task has dropped its MM,
		// FDs and mounts. Include forked children, even after they are reaped.
		process.tasks.Wait()
		err = errors.Join(err, releaseSyscallMemory(k, id))
		s.platform.mu.Lock()
		delete(s.platform.processes, id)
		s.platform.mu.Unlock()
	}()
	ls, err := limits.NewLinuxLimitSet()
	if err != nil {
		return err
	}
	args := kernel.CreateProcessArgs{
		Filename: executable, Argv: []string{executable}, Envv: env,
		WorkingDirectory: "/",
		Credentials:      auth.NewUserCredentials(1000, 1000, nil, &auth.TaskCapabilities{}, k.RootUserNamespace()),
		Umask:            0022, Limits: ls,
		MaxSymlinkTraversals: linux.MaxSymlinkTraversals,
		UTSNamespace:         k.RootUTSNamespace(), IPCNamespace: k.RootIPCNamespace(),
		PIDNamespace: pidns, ContainerID: id,
	}
	ctx := mountContext{args.NewContext(k)}
	mntns, err := mountFilesystem(ctx, k, mounts)
	if err != nil {
		return err
	}
	defer mntns.DecRef(ctx)

	fdt, err := importDescriptors(k, imageFD)
	if err != nil {
		return err
	}
	defer fdt.DecRef(ctx)
	args.FDTable, args.MountNamespace = fdt, mntns
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	// CreateProcess takes ownership of one mount-namespace reference. The local
	// reference above remains valid for cleanup on both success and failure.
	mntns.IncRef()
	tg, _, err := k.CreateProcess(args)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("loading guest: %w", err)
	}

	// Patch only the guest's private ELF mapping, before any guest task runs.
	// Go initializes its runtime and packages, then main.main branches to entryPC.
	_, err = tg.Leader().MemoryManager().CopyOut(ctx, hostarch.Addr(mainPC), jump, usermem.IOOpts{IgnorePermissions: true})
	if err != nil {
		// Even an unstarted task must enter the task loop to release its refs.
		_ = tg.SendSignal(&linux.SignalInfo{Signo: int32(linux.SIGKILL)})
	}
	k.StartProcess(tg)
	s.mu.Unlock()
	tg.WaitExited()
	process.tasks.Wait()
	if err != nil {
		return fmt.Errorf("installing guest entry: %w", err)
	}

	status := tg.ExitStatus()
	if !status.Exited() || status.ExitStatus() != 0 {
		return fmt.Errorf("application exited with %s", status)
	}
	return nil
}
