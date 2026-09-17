//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/xgo-dev/sandbox/internal/state"
	"golang.org/x/sys/unix"
)

func TestStateImageRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name       string
		inputSize  int
		outputSize int
	}{
		{"small", 3, 5},
		{"large_input", 17 << 20, 3},
		{"large_output", 3, 19 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			fd, err := newStateImage()
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			var host, guest state.State
			input := strings.Repeat("a", test.inputSize)
			offset, err := writeStateImage(fd, 0, &host, &input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readStateImage(fd, offset); err == nil {
				t.Fatal("unpublished output accepted")
			}
			data, err := readStateImage(fd, 0)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(data))+8 != offset {
				t.Fatalf("input header: payload=%d offset=%d", len(data), offset)
			}
			var output string
			if _, err := guest.Load(context.Background(), data, &output); err != nil {
				t.Fatal(err)
			}
			if output != input {
				t.Fatal("input changed during transfer")
			}
			output = strings.Repeat("b", test.outputSize)
			resultSize, err := writeStateImage(fd, offset, &guest, &output)
			if err != nil {
				t.Fatal(err)
			}
			result, err := readStateImage(fd, offset)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(result))+8 != resultSize {
				t.Fatal("result header does not describe its actual size")
			}
			var stat unix.Stat_t
			if err := unix.Fstat(fd, &stat); err != nil {
				t.Fatal(err)
			}
			if stat.Size != offset+resultSize+8 {
				t.Fatalf("memfd size=%d, want %d", stat.Size, offset+resultSize+8)
			}
			// A changed input header must not redirect the host's result read.
			if _, err := unix.Pwrite(fd, make([]byte, 8), 0); err != nil {
				t.Fatal(err)
			}
			if _, err := host.Load(context.Background(), result, &input); err != nil {
				t.Fatal(err)
			}
			if input != output {
				t.Fatal("result was not written back")
			}
			if _, err := readStateImage(fd, offset); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStateImageLengths(t *testing.T) {
	fd, err := newStateImage()
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err := readStateImage(fd, 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("empty file: %v", err)
	}
	if err := unix.Ftruncate(fd, 32); err != nil {
		t.Fatal(err)
	}
	for _, size := range []uint64{0, 7, 33, uint64(math.MaxInt64) + 1, math.MaxUint64} {
		var header [8]byte
		binary.LittleEndian.PutUint64(header[:], size)
		if _, err := unix.Pwrite(fd, header[:], 0); err != nil {
			t.Fatal(err)
		}
		if _, err := readStateImage(fd, 0); err == nil {
			t.Fatalf("accepted invalid length %d", size)
		}
	}
	if err := unix.Ftruncate(fd, 16); !errors.Is(err, unix.EPERM) {
		t.Fatalf("image was not protected against truncation: %v", err)
	}
	if err := unix.Ftruncate(fd, 64); err != nil {
		t.Fatalf("image could not grow: %v", err)
	}
}
