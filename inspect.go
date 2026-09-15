package sandbox

import (
	"fmt"
	"sync"
)

type syscallAccess struct {
	mu     sync.Mutex
	active bool
	mmap   func(uint64, int) (Memory, error)
}

// Memory is an independent syscall input stored in Sentry-managed pages.
// Data directly maps those pages in the host; Addr identifies them in the guest.
// Data is borrowed until Inspect returns and must not contain host Go pointers.
type Memory struct {
	Data []byte
	Addr uint64
}

// MMap allocates size bytes of temporary Sentry memory and returns a writable
// mapping. Address zero allocates zeroed memory; a nonzero address copies guest
// bytes into it. Editing Data does not change the original memory.
// The caller explicitly assigns Addr to syscall arguments or nested pointers;
// returning from Inspect resumes execution without a separate commit.
//
// A partial read returns a shorter Data slice and an error. Concurrent guest
// writes are not an atomic snapshot. Temporary addresses must not escape the
// synchronous syscall or be unmapped/remapped by guest threads.
func (s *Syscall) MMap(address uint64, size int) (Memory, error) {
	if s.access == nil {
		return Memory{}, fmt.Errorf("sandbox: syscall is not inside Inspect")
	}
	a := s.access
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.active {
		return Memory{}, fmt.Errorf("sandbox: Inspect has returned")
	}
	if size < 0 {
		return Memory{}, fmt.Errorf("sandbox: negative memory size")
	}
	if size == 0 {
		return Memory{}, nil
	}
	return a.mmap(address, size)
}
