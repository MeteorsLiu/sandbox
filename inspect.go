package sandbox

import (
	"fmt"
	"sync"
)

type syscallAccess struct {
	mu     sync.Mutex
	active bool
	read   func(uint64, []byte) (int, error)
}

// ReadMemory copies guest memory while Inspect is running. It respects guest
// read permissions and may return both a partial count and an error. The copy
// is not an atomic snapshot of memory shared with other guest threads.
func (s *Syscall) ReadMemory(address uint64, dst []byte) (int, error) {
	if s.access == nil {
		return 0, fmt.Errorf("sandbox: syscall is not inside Inspect")
	}
	a := s.access
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.active {
		return 0, fmt.Errorf("sandbox: Inspect has returned")
	}
	return a.read(address, dst)
}
