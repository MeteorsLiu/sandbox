package sandbox

import (
	"errors"
	"io"
	"testing"
)

func TestReadMemoryAndLifetime(t *testing.T) {
	reads := 0
	access := &syscallAccess{
		active: true,
		read: func(address uint64, dst []byte) (int, error) {
			reads++
			if address != 0x1000 {
				t.Fatalf("wrong guest address: %#x", address)
			}
			return copy(dst, "data"), io.ErrUnexpectedEOF
		},
	}
	call := &Syscall{Number: 1, Name: "test", access: access}
	if call.Name != "test" || reads != 0 {
		t.Fatal("filtering unexpectedly read guest memory")
	}
	var buf [8]byte
	n, err := call.ReadMemory(0x1000, buf[:])
	if n != 4 || string(buf[:n]) != "data" || !errors.Is(err, io.ErrUnexpectedEOF) || reads != 1 {
		t.Fatalf("partial read was not preserved: %d %q %v", n, buf, err)
	}
	access.active = false
	if _, err := call.ReadMemory(0x1000, buf[:]); err == nil || reads != 1 {
		t.Fatal("retained callback could access guest memory")
	}
	if _, err := new(Syscall).ReadMemory(0x1000, buf[:]); err == nil {
		t.Fatal("event without an inspector could access guest memory")
	}
}
