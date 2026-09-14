//go:build linux && (arm64 || amd64) && cgo

package main

import (
	"errors"
	"fmt"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/timing"
)

func runSentry(root, executable string, imageFD int, inspect inspector) error {
	if executable == "" {
		return errors.New("guest executable is required")
	}
	ioFD, stopFilesystem, err := startFilesystem(root)
	if err != nil {
		return err
	}
	defer stopFilesystem()
	defer ioFD.Close()

	k, err := newKernel(inspect)
	if err != nil {
		return err
	}
	defer k.Release()
	ctx := k.SupervisorContext()

	mntns, err := mountFilesystem(k, ioFD.Release())
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
	timeline := timing.New("llar-runtime", time.Now()).Fork("guest")
	defer timeline.End()

	// CreateProcess takes ownership of one mount-namespace reference. The local
	// reference above remains valid for cleanup on both success and failure.
	mntns.IncRef()
	tg, _, err := k.CreateProcess(kernel.CreateProcessArgs{
		Filename: executable, Argv: []string{executable, "--llar-sandbox-guest"},
		Envv: []string{"GOMAXPROCS=2"}, WorkingDirectory: "/",
		Credentials: auth.NewUserCredentials(1000, 1000, nil, &auth.TaskCapabilities{}, k.RootUserNamespace()),
		FDTable:     fdt, Umask: 0022, Limits: ls,
		MaxSymlinkTraversals: linux.MaxSymlinkTraversals,
		UTSNamespace:         k.RootUTSNamespace(), IPCNamespace: k.RootIPCNamespace(),
		PIDNamespace: k.RootPIDNamespace(), MountNamespace: mntns,
		StartupTimeline: timeline,
	})
	if err != nil {
		return fmt.Errorf("loading guest: %w", err)
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
