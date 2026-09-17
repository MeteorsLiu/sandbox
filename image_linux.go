//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/xgo-dev/sandbox/internal/state"
	"golang.org/x/sys/unix"
)

func newStateImage() (int, error) {
	fd, err := unix.MemfdCreate("llar-sandbox", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return -1, err
	}
	// The result may grow, but neither participant may truncate a live mapping
	// or change the seals after the descriptor is handed to the guest.
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// Each image starts with its total byte length, including the eight-byte header.
// The input and result occupy consecutive images in the same memfd. The host
// retains the result offset independently of the guest-writable input header.
func writeStateImage(fd int, offset int64, graph *state.State, root any) (int64, error) {
	data := make([]byte, 1<<20)
	var n int
	for {
		var err error
		n, _, err = graph.Save(context.Background(), data, root)
		if err == nil {
			break
		}
		if !errors.Is(err, io.ErrShortBuffer) {
			return 0, err
		}
		if len(data) > (math.MaxInt-16)/2 {
			return 0, fmt.Errorf("sandbox image exceeds addressable memory")
		}
		data = make([]byte, 2*len(data))
	}
	// Reserve the following header as well. A guest that exits without writing
	// its result leaves this zero, so the host cannot accept the input as output.
	if offset < 0 || n > math.MaxInt-16 || offset > math.MaxInt64-int64(n)-16 {
		return 0, fmt.Errorf("sandbox image size overflow")
	}
	size := int64(n) + 8
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return 0, err
	}
	if end := offset + size + 8; end > stat.Size {
		if err := unix.Ftruncate(fd, end); err != nil {
			return 0, err
		}
	}
	mapOffset := offset - offset%int64(unix.Getpagesize())
	delta := int(offset - mapOffset)
	if n > math.MaxInt-delta-16 {
		return 0, fmt.Errorf("sandbox mapping size overflow")
	}
	mem, err := unix.Mmap(fd, mapOffset, delta+n+16, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return 0, err
	}
	image := mem[delta:]
	binary.LittleEndian.PutUint64(image, 0)
	copy(image[8:], data[:n])
	binary.LittleEndian.PutUint64(image[8+n:], 0)
	// Publish the size only after the payload is complete. The reader must also
	// wait for ownership to transfer; this header is not a synchronization lock.
	binary.LittleEndian.PutUint64(image, uint64(size))
	if err := unix.Munmap(mem); err != nil {
		return 0, err
	}
	return size, nil
}

// The sender must have finished before reading. Copy before decoding, and keep
// no references to the shared mapping while restoring the host object graph.
func readStateImage(fd int, offset int64) ([]byte, error) {
	var header [8]byte
	n, err := unix.Pread(fd, header[:], offset)
	if err != nil {
		return nil, err
	}
	if n != len(header) {
		return nil, io.ErrUnexpectedEOF
	}
	size := binary.LittleEndian.Uint64(header[:])
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if offset < 0 || offset > stat.Size || size < 8 || size > uint64(stat.Size-offset) {
		return nil, fmt.Errorf("invalid or incomplete sandbox image length %d", size)
	}
	mapOffset := offset - offset%int64(unix.Getpagesize())
	delta := int(offset - mapOffset)
	if size > uint64(math.MaxInt-delta) {
		return nil, fmt.Errorf("sandbox mapping size overflow")
	}
	mem, err := unix.Mmap(fd, mapOffset, delta+int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	data := append([]byte(nil), mem[delta+8:delta+int(size)]...)
	if err := unix.Munmap(mem); err != nil {
		return nil, err
	}
	return data, nil
}
