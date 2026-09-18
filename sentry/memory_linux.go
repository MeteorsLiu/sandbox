//go:build linux && (amd64 || arm64) && cgo

package main

import (
	"errors"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/usermem"
)

type syscallMemoryRegion struct {
	address hostarch.Addr
	length  uint64
	pins    []mm.PinnedRange
	mapping []byte
	owned   bool
}

type syscallMemory struct {
	task    *kernel.Task
	ctx     context.Context
	mm      *mm.MemoryManager
	regions []syscallMemoryRegion
}

func (m *syscallMemory) mmap(address hostarch.Addr, size uint64) (data []byte, guest hostarch.Addr, err error) {
	if size == 0 {
		return nil, 0, nil
	}
	length, ok := hostarch.Addr(size).RoundUp()
	if !ok || uint64(length) > uint64(^uint(0)>>1) {
		return nil, 0, linuxerr.EINVAL
	}
	var source hostarch.AddrRange
	if address != 0 {
		source, ok = m.mm.CheckIORange(address, int64(size))
		if !ok {
			return nil, 0, linuxerr.EFAULT
		}
	}
	addr, err := m.mm.MMap(m.task, memmap.MMapOpts{
		Length: uint64(length), Private: true,
		Perms: hostarch.ReadWrite, MaxPerms: hostarch.ReadWrite,
	})
	if err != nil {
		return nil, 0, err
	}
	r := syscallMemoryRegion{address: addr, length: uint64(length)}
	retained := false
	defer func() {
		if !retained {
			err = errors.Join(err, r.release(m))
		}
	}()
	temporary := hostarch.AddrRange{Start: addr, End: addr + length}
	// A vacant source address can be selected by MMap. Do not let this new
	// mapping make an invalid source readable (for example, after munmap).
	if address != 0 && temporary.Overlaps(source) {
		return nil, 0, linuxerr.EFAULT
	}
	r.pins, err = m.mm.Pin(m.task, temporary, hostarch.ReadWrite, false)
	if err != nil {
		return nil, 0, err
	}
	r.mapping, r.owned, err = mapTemporaryMemory(r.pins, int(length))
	if err != nil {
		return nil, 0, err
	}
	// Anonymous pages are initially zeroed. A nonzero source initializes them
	// directly; the host receives an alias of these same temporary pages.
	n := int(size)
	if address != 0 {
		n, err = m.mm.CopyIn(m.ctx, address, r.mapping[:n], usermem.IOOpts{})
		if n == 0 && err != nil {
			return nil, 0, err
		}
	}
	if len(m.regions) == 0 {
		m.mm.IncUsers()
	}
	m.regions = append(m.regions, r)
	retained = true
	return r.mapping[:n:n], addr, err
}

// Internal mappings can span discontiguous MemoryFile chunks. In that case,
// reserve a host range and map the pinned pages into it without copying them.
func mapTemporaryMemory(pins []mm.PinnedRange, length int) ([]byte, bool, error) {
	if len(pins) == 1 {
		blocks, err := pins[0].File.MapInternal(pins[0].FileRange(), hostarch.ReadWrite)
		if err != nil {
			return nil, false, err
		}
		if blocks.NumBlocks() == 1 {
			return blocks.Head().ToSlice(), false, nil
		}
	}
	mapping, err := unix.Mmap(-1, 0, length, unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return nil, false, err
	}
	base := unsafe.Pointer(unsafe.SliceData(mapping))
	for _, pin := range pins {
		fd, err := pin.File.DataFD(pin.FileRange())
		if err == nil {
			offset := uintptr(pin.Source.Start - pins[0].Source.Start)
			_, err = unix.MmapPtr(fd, int64(pin.Offset), unsafe.Add(base, offset), uintptr(pin.Source.Length()), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_FIXED)
		}
		if err != nil {
			return nil, false, errors.Join(err, unix.Munmap(mapping))
		}
	}
	return mapping, true, nil
}

func (r *syscallMemoryRegion) release(m *syscallMemory) error {
	err := m.mm.MUnmap(m.ctx, r.address, r.length)
	if r.owned {
		err = errors.Join(err, unix.Munmap(r.mapping))
	}
	mm.Unpin(r.pins)
	return err
}

var memoryHooks sync.Once
var memoryCalls = struct {
	sync.Mutex
	pending map[*kernel.Task]*syscallMemory
	live    map[*syscallMemory]struct{}
}{pending: make(map[*kernel.Task]*syscallMemory), live: make(map[*syscallMemory]struct{})}

// Install once before any task starts. Init refreshes the table's private fast
// lookup; it must not run again after syscall feature flags have been enabled.
func installSyscallMemory() {
	memoryHooks.Do(func() {
		for _, table := range kernel.SyscallTables() {
			for number, syscall := range table.Table {
				fn := syscall.Fn
				if fn == nil {
					continue
				}
				syscall.Fn = func(t *kernel.Task, number uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
					memoryCalls.Lock()
					m := memoryCalls.pending[t]
					delete(memoryCalls.pending, t)
					memoryCalls.Unlock()
					value, control, err := fn(t, number, args)
					if m != nil && !linuxerr.IsRestartError(err) {
						if releaseErr := finishSyscallMemory(m); releaseErr != nil {
							t.Warningf("releasing syscall memory: %v", releaseErr)
						}
					}
					return value, control, err
				}
				table.Table[number] = syscall
			}
			table.Init()
		}
	})
}

func (m *syscallMemory) release() error {
	var err error
	for i := range m.regions {
		err = errors.Join(err, m.regions[i].release(m))
	}
	m.mm.DecUsers(m.ctx)
	return err
}

func finishSyscallMemory(m *syscallMemory) error {
	memoryCalls.Lock()
	delete(memoryCalls.live, m)
	if memoryCalls.pending[m.task] == m {
		delete(memoryCalls.pending, m.task)
	}
	memoryCalls.Unlock()
	return m.release()
}

// Restart blocks and signal frames can retain addresses after an execution
// attempt ends. Keep those mappings, and calls skipped before dispatch, until
// this Run has stopped all its tasks. The extra MM user also covers successful
// execve. Other concurrent Runs retain their own mappings.
func releaseSyscallMemory(k *kernel.Kernel, processID string) error {
	memoryCalls.Lock()
	var remaining []*syscallMemory
	for m := range memoryCalls.live {
		if m.task.Kernel() == k && m.task.ContainerID() == processID {
			remaining = append(remaining, m)
		}
	}
	memoryCalls.Unlock()
	var err error
	for _, m := range remaining {
		err = errors.Join(err, finishSyscallMemory(m))
	}
	return err
}
