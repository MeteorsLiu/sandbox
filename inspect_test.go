package sandbox

import (
	"errors"
	"io"
	"testing"
)

func TestMMapAndLifetime(t *testing.T) {
	reads := 0
	backing := []byte("data")
	access := &syscallAccess{
		active: true,
		mmap: func(address uint64, size int) (Memory, error) {
			reads++
			if address != 0x1000 || size != 8 {
				t.Fatalf("wrong guest range: %#x, %d", address, size)
			}
			return Memory{Data: backing, Addr: 0x2000}, io.ErrUnexpectedEOF
		},
	}
	call := &Syscall{Number: 1, Name: "test", Args: [6]uint64{0x1000, 0x1000}, access: access}
	if call.Name != "test" || reads != 0 {
		t.Fatal("filtering unexpectedly read guest memory")
	}
	view, err := call.MMap(0x1000, 8)
	if len(view.Data) != 4 || string(view.Data) != "data" || view.Addr != 0x2000 || !errors.Is(err, io.ErrUnexpectedEOF) || reads != 1 {
		t.Fatalf("partial view was not preserved: %+v, %v", view, err)
	}
	view.Data[0] = 'D'
	if string(backing) != "Data" || call.Args != [6]uint64{0x1000, 0x1000} {
		t.Fatal("view was copied or raw arguments were automatically rewritten")
	}
	if view, err := call.MMap(0, 0); err != nil || len(view.Data) != 0 || view.Addr != 0 || reads != 1 {
		t.Fatal("zero-length read allocated memory")
	}
	if _, err := call.MMap(0, -1); err == nil || reads != 1 {
		t.Fatal("negative size reached the backend")
	}
	access.active = false
	if _, err := call.MMap(0x1000, 8); err == nil || reads != 1 {
		t.Fatal("retained callback could access guest memory")
	}
	if _, err := new(Syscall).MMap(0x1000, 8); err == nil {
		t.Fatal("event without an inspector could access guest memory")
	}
}
