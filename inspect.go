package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
)

// Argument is an argument classified by gVisor's strace table. Value contains
// a JSON number, string, array, or object. Decoded is false when the argument
// lacks an input codec (including output buffers at syscall entry). Supported
// scalar arguments are decoded as numbers without reading guest memory.
type Argument struct {
	Format  string `json:"format"`
	Value   any    `json:"value"`
	Decoded bool   `json:"decoded"`
}

// DecodedSyscall contains editable syscall arguments, in Linux ABI order.
// Integers are json.Number to preserve all 64 bits. Argument formats and the
// syscall identity must not be changed; use Syscall.Number/Args for raw edits.
type DecodedSyscall struct {
	Number uint64     `json:"number"`
	Name   string     `json:"name"`
	Args   []Argument `json:"args"`
}

type syscallAccess struct {
	mu      sync.Mutex
	active  bool
	read    func(uint64, []byte) (int, error)
	decode  func(*Syscall, int) ([]byte, error)
	rewrite func(*Syscall, []byte) error
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

// Decode reads arguments on demand. maxBytes bounds the total guest data read;
// exceeding it returns an error rather than truncating values. A later Decode
// replaces the snapshot against which Rewrite compares edits.
func (s *Syscall) Decode(maxBytes int) (*DecodedSyscall, error) {
	if s.access == nil {
		return nil, fmt.Errorf("sandbox: syscall is not inside Inspect")
	}
	a := s.access
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.active {
		return nil, fmt.Errorf("sandbox: Inspect has returned")
	}
	if maxBytes < 0 {
		return nil, fmt.Errorf("sandbox: negative decode budget")
	}
	data, err := a.decode(s, maxBytes)
	if err != nil {
		return nil, err
	}
	var call DecodedSyscall
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&call); err != nil {
		return nil, fmt.Errorf("sandbox: decode Sentry arguments: %w", err)
	}
	return &call, nil
}

// Rewrite applies edits to the last Decode result. Sentry encodes input values
// in guest memory and updates Args. Replacement mappings live until the guest
// address space is destroyed, so interrupted syscalls can safely restart.
// Errors leave the syscall registers unchanged. This does not deny a syscall.
func (s *Syscall) Rewrite(call *DecodedSyscall) error {
	if s.access == nil {
		return fmt.Errorf("sandbox: syscall is not inside Inspect")
	}
	a := s.access
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.active {
		return fmt.Errorf("sandbox: Inspect has returned")
	}
	if call == nil {
		return fmt.Errorf("sandbox: nil decoded syscall")
	}
	data, err := json.Marshal(call)
	if err != nil {
		return fmt.Errorf("sandbox: encode edited arguments: %w", err)
	}
	return a.rewrite(s, data)
}
