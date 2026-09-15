//go:build linux && (arm64 || amd64) && cgo

package main

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/usermem"
)

func runSentry(mounts []mount, executable string, imageFD int, mainPC, entryPC uintptr, inspect inspector) (err error) {
	if !path.IsAbs(executable) || strings.ContainsRune(executable, 0) {
		return errors.New("guest executable must be an absolute path without NUL")
	}
	jump, err := entryJump(mainPC, entryPC)
	if err != nil {
		return err
	}
	defer closeMounts(mounts)
	if err := prepareMounts(mounts); err != nil {
		return err
	}

	k, err := newKernel(inspect)
	if err != nil {
		return err
	}
	defer k.Release()
	defer func() { err = errors.Join(err, releaseSyscallMemory(k)) }()
	ctx := k.SupervisorContext()

	mntns, err := mountFilesystem(k, mounts)
	if err != nil {
		return err
	}
	defer mntns.DecRef(ctx)

	fdt, err := importDescriptors(k, imageFD)
	if err != nil {
		return err
	}
	defer fdt.DecRef(ctx)
	ls, err := limits.NewLinuxLimitSet()
	if err != nil {
		return err
	}
	// CreateProcess takes ownership of one mount-namespace reference. The local
	// reference above remains valid for cleanup on both success and failure.
	mntns.IncRef()
	tg, _, err := k.CreateProcess(kernel.CreateProcessArgs{
		Filename: executable, Argv: []string{executable},
		WorkingDirectory: "/",
		Credentials:      auth.NewUserCredentials(1000, 1000, nil, &auth.TaskCapabilities{}, k.RootUserNamespace()),
		FDTable:          fdt, Umask: 0022, Limits: ls,
		MaxSymlinkTraversals: linux.MaxSymlinkTraversals,
		UTSNamespace:         k.RootUTSNamespace(), IPCNamespace: k.RootIPCNamespace(),
		PIDNamespace: k.RootPIDNamespace(), MountNamespace: mntns,
	})
	if err != nil {
		return fmt.Errorf("loading guest: %w", err)
	}

	// Patch only the guest's private ELF mapping, before any guest task runs.
	// Go initializes its runtime and packages, then main.main branches to entryPC.
	_, err = tg.Leader().MemoryManager().CopyOut(ctx, hostarch.Addr(mainPC), jump, usermem.IOOpts{IgnorePermissions: true})
	if err != nil {
		return fmt.Errorf("installing guest entry: %w", err)
	}

	dog := watchdog.New(k, watchdog.DefaultOpts)
	dog.Start()
	defer dog.Stop()
	if err := k.Start(); err != nil {
		return err
	}
	k.WaitExited()
	status := tg.ExitStatus()
	if !status.Exited() || status.ExitStatus() != 0 {
		return fmt.Errorf("application exited with %s", status)
	}
	return nil
}
