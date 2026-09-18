//go:build linux && (arm64 || amd64)

package main

import (
	"sync"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

type inspector func(context.Context, *arch.Context64) error

type observedPlatform struct {
	platform.Platform
	mu        sync.Mutex
	processes map[string]*guestProcess
}

func (p *observedPlatform) NewContext(ctx context.Context) platform.Context {
	task := kernel.TaskFromContext(ctx)
	p.mu.Lock()
	process := p.processes[task.ContainerID()]
	process.tasks.Add(1)
	p.mu.Unlock()
	return &observedContext{Context: p.Platform.NewContext(ctx), process: process}
}

type observedContext struct {
	platform.Context
	process *guestProcess
}

func (c *observedContext) Release() {
	c.Context.Release()
	c.process.tasks.Done()
}

func (c *observedContext) Switch(ctx context.Context, mm platform.MemoryManager, ac *arch.Context64, cpu int32) (*linux.SignalInfo, hostarch.AccessType, error) {
	info, access, err := c.Context.Switch(ctx, mm, ac, cpu)
	if err == nil {
		ac.SyscallSaveOrig()
		if err := c.process.inspect(ctx, ac); err != nil {
			return nil, hostarch.NoAccess, err
		}
	}
	return info, access, err
}
