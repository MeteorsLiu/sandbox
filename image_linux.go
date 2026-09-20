//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"bufio"
	"context"
	"encoding/binary"
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
	if offset < 0 || offset > math.MaxInt64-16 {
		return 0, fmt.Errorf("sandbox image size overflow")
	}
	output := imageWriter{fd: fd, offset: offset}
	var header [8]byte
	if _, err := output.Write(header[:]); err != nil {
		return 0, err
	}
	buffer := bufio.NewWriter(&output)
	n, _, err := graph.SaveTo(context.Background(), buffer, root)
	if err != nil {
		return 0, err
	}
	if err := buffer.Flush(); err != nil {
		return 0, err
	}
	// An unwritten result must keep its following header unpublished.
	if _, err := output.Write(header[:]); err != nil {
		return 0, err
	}
	size := int64(n) + 8
	// Publish the size only after the payload is complete. The reader must also
	// wait for ownership to transfer; this header is not a synchronization lock.
	binary.LittleEndian.PutUint64(header[:], uint64(size))
	output.offset = offset
	if _, err := output.Write(header[:]); err != nil {
		return 0, err
	}
	return size, nil
}

// imageWriter keeps the input and result independent of the shared fd offset.
type imageWriter struct {
	fd     int
	offset int64
}

func (w *imageWriter) Write(p []byte) (int, error) {
	if w.offset < 0 || w.offset > math.MaxInt64-int64(len(p)) {
		return 0, fmt.Errorf("sandbox image size overflow")
	}
	for {
		n, err := unix.Pwrite(w.fd, p, w.offset)
		if err == unix.EINTR {
			continue
		}
		n = max(n, 0)
		w.offset += int64(n)
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
		return n, err
	}
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
