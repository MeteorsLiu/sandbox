//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
)

type codecMemory struct {
	blocks map[uint64][]byte
	reads  int
	next   uint64
}

func (m *codecMemory) read(address uint64, dst []byte) (int, error) {
	m.reads++
	if len(dst) == 0 {
		return 0, nil
	}
	for base, data := range m.blocks {
		if address >= base && address-base < uint64(len(data)) {
			n := copy(dst, data[address-base:])
			if n < len(dst) {
				return n, io.EOF
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("unmapped address %#x", address)
}

func (m *codecMemory) allocate(data []byte, fixups []pointerFixup) (uint64, error) {
	if m.next == 0 {
		m.next = 0x10000
	}
	address := m.next
	m.next += uint64(len(data)) + 4096
	data = bytes.Clone(data)
	for _, p := range fixups {
		binary.LittleEndian.PutUint64(data[p.at:], address+uint64(p.target))
	}
	m.blocks[address] = data
	return address, nil
}

func syscallNumber(t *testing.T, name string) uint64 {
	t.Helper()
	for number, info := range syscallFormats {
		if info.name == name {
			return number
		}
	}
	t.Fatalf("missing generated syscall %s", name)
	return 0
}

func decodedCall(t *testing.T, c *syscallCodec, number uint64, args [6]uint64) decodedSyscall {
	t.Helper()
	data, err := c.decode(number, args, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var call decodedSyscall
	if err := json.Unmarshal(data, &call); err != nil {
		t.Fatal(err)
	}
	return call
}

func rewriteCall(t *testing.T, c *syscallCodec, args [6]uint64, call decodedSyscall) [6]uint64 {
	t.Helper()
	data, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.rewrite(call.Number, args, data)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCodecPathRewrite(t *testing.T) {
	m := &codecMemory{blocks: map[uint64][]byte{0x1000: []byte("/a\x00neighbor")}}
	c := &syscallCodec{memory: m}
	args := [6]uint64{0xffffffffffffff9c, 0x1000, 0, 0}
	call := decodedCall(t, c, syscallNumber(t, "openat"), args)
	if string(call.Args[0].Value) != "-100" || string(call.Args[1].Value) != `"/a"` {
		t.Fatalf("unexpected decode: %+v", call)
	}
	call.Args[1].Value = json.RawMessage(`"/build/source/a"`)
	call.Args[2].Value = json.RawMessage(`524288`)
	out := rewriteCall(t, c, args, call)
	if out[0] != args[0] || out[2] != linux.O_CLOEXEC || out[1] == args[1] {
		t.Fatalf("bad rewritten registers: %x", out)
	}
	if !bytes.Equal(m.blocks[out[1]], []byte("/build/source/a\x00")) || string(m.blocks[0x1000]) != "/a\x00neighbor" {
		t.Fatal("replacement corrupted the original pathname or neighbors")
	}
}

func TestCodecVectorsAndBytes(t *testing.T) {
	m := &codecMemory{blocks: map[uint64][]byte{0x1000: []byte("/bin/tool\x00")}}
	c := &syscallCodec{memory: m}
	args := [6]uint64{0x1000, 0, 0}
	call := decodedCall(t, c, syscallNumber(t, "execve"), args)
	call.Args[1].Value = json.RawMessage(`["tool", "-c", "source.c"]`)
	call.Args[2].Value = json.RawMessage(`["CC=clang", "PATH=/bin"]`)
	out := rewriteCall(t, c, args, call)
	again := decodedCall(t, c, call.Number, out)
	if !sameJSON(again.Args[1].Value, call.Args[1].Value) || !sameJSON(again.Args[2].Value, call.Args[2].Value) {
		t.Fatalf("vector pointer fixups did not round trip: %s %s", again.Args[1].Value, again.Args[2].Value)
	}

	m.blocks[0x2000] = []byte{'a', 0, 0xff}
	args = [6]uint64{1, 0x2000, 3}
	call = decodedCall(t, c, syscallNumber(t, "write"), args)
	var value byteValue
	if err := json.Unmarshal(call.Args[1].Value, &value); err != nil || !bytes.Equal(value.Bytes, m.blocks[0x2000]) {
		t.Fatalf("binary input lost data: %s", call.Args[1].Value)
	}
	call.Args[1].Value, _ = json.Marshal(byteValue{Bytes: []byte("longer write")})
	out = rewriteCall(t, c, args, call)
	if out[2] != 12 || string(m.blocks[out[1]]) != "longer write" {
		t.Fatalf("buffer/count rewrite: %x", out)
	}

	m.blocks[0x3000] = []byte{'/', 0xff, 0}
	args = [6]uint64{0, 0x3000}
	call = decodedCall(t, c, syscallNumber(t, "openat"), args)
	s, err := parseString(call.Args[1].Value)
	if err != nil || s != string([]byte{'/', 0xff}) {
		t.Fatalf("non-UTF-8 path was lost: %s %v", call.Args[1].Value, err)
	}
}

func TestCodecLimitsAndInvalidEdits(t *testing.T) {
	m := &codecMemory{blocks: map[uint64][]byte{0x1000: []byte("/some/path\x00")}}
	c := &syscallCodec{memory: m}
	number := syscallNumber(t, "openat")
	args := [6]uint64{0, 0x1000}
	if _, err := c.decode(number, args, 3); err == nil {
		t.Fatal("oversized path silently truncated")
	}
	bad := args
	bad[1] = 1
	if _, err := c.decode(number, bad, 4096); err == nil {
		t.Fatal("unmapped path accepted")
	}
	for _, value := range []string{`null`, `1.5`, `18446744073709551616`, `-1`, `"1"`} {
		call := decodedCall(t, c, number, args)
		call.Args[1].Value = json.RawMessage(`"/new"`)
		call.Args[2].Value = json.RawMessage(value)
		data, _ := json.Marshal(call)
		out, err := c.rewrite(number, args, data)
		if err == nil || out != args || len(m.blocks) != 1 {
			t.Fatalf("invalid scalar %s caused a partial rewrite: %x %v", value, out, err)
		}
	}
	call := decodedCall(t, c, number, args)
	call.Args[1].Value, _ = json.Marshal("/bad\x00path")
	data, _ := json.Marshal(call)
	if _, err := c.rewrite(number, args, data); err == nil {
		t.Fatal("embedded NUL accepted")
	}
}

func TestCodecOverloadedAndOutputArguments(t *testing.T) {
	m := &codecMemory{blocks: map[uint64][]byte{0x1000: []byte("/a\x00")}}
	c := &syscallCodec{memory: m}
	for _, test := range []struct {
		name  string
		args  [6]uint64
		index int
	}{
		{"ppoll", [6]uint64{0, 0, 0xdeadbeef}, 2},
		{"futex", [6]uint64{0, linux.FUTEX_REQUEUE, 1, 2}, 3},
		{"epoll_ctl", [6]uint64{1, linux.EPOLL_CTL_DEL, 2, 0xdeadbeef}, 3},
		{"listxattr", [6]uint64{0x1000, 0xdeadbeef, 100}, 1},
		{"mount", [6]uint64{0x1000, 0x1000, 0x1000, 0, 0xdeadbeef}, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			call := decodedCall(t, c, syscallNumber(t, test.name), test.args)
			if call.Args[test.index].Decoded {
				t.Fatal("output or overloaded argument decoded as a fixed input")
			}
		})
	}

	times := []linux.Timespec{{Sec: 10, Nsec: 20}, {Sec: 30, Nsec: 40}}
	data := make([]byte, 2*times[0].SizeBytes())
	times[0].MarshalBytes(data)
	times[1].MarshalBytes(data[times[0].SizeBytes():])
	m.blocks[0x2000] = data
	args := [6]uint64{0, 0x1000, 0x2000}
	call := decodedCall(t, c, syscallNumber(t, "utimensat"), args)
	expected, _ := json.Marshal(times)
	if !sameJSON(call.Args[2].Value, expected) {
		t.Fatalf("utimensat lost one of its timestamps: %s", call.Args[2].Value)
	}
	times[1].Nsec = 50
	call.Args[2].Value, _ = json.Marshal(times)
	out := rewriteCall(t, c, args, call)
	again := decodedCall(t, c, call.Number, out)
	if !sameJSON(again.Args[2].Value, call.Args[2].Value) {
		t.Fatal("utimensat timestamps did not round trip")
	}
}

func TestCodecStructAndOutputDirection(t *testing.T) {
	how := linux.OpenHow{Flags: linux.O_CLOEXEC, Mode: 0644}
	data := make([]byte, how.SizeBytes())
	how.MarshalBytes(data)
	m := &codecMemory{blocks: map[uint64][]byte{0x1000: []byte("/a\x00"), 0x2000: data}}
	c := &syscallCodec{memory: m}
	args := [6]uint64{0, 0x1000, 0x2000, uint64(len(data))}
	call := decodedCall(t, c, syscallNumber(t, "openat2"), args)
	if !call.Args[2].Decoded {
		t.Fatal("open_how left raw")
	}
	how.Flags = linux.O_RDONLY
	call.Args[2].Value, _ = json.Marshal(&how)
	out := rewriteCall(t, c, args, call)
	var rewritten linux.OpenHow
	rewritten.UnmarshalBytes(m.blocks[out[2]])
	if rewritten != how {
		t.Fatalf("open_how rewrite: %+v", rewritten)
	}
	args[3]++
	call = decodedCall(t, c, call.Number, args)
	if call.Args[2].Decoded {
		t.Fatal("unknown open_how extension was discarded")
	}

	before := m.reads
	args = [6]uint64{7, 0xdeadbeef, 4096}
	call = decodedCall(t, c, syscallNumber(t, "read"), args)
	if call.Args[1].Decoded || m.reads != before {
		t.Fatal("read output was read at syscall entry")
	}
	call = decodedCall(t, c, ^uint64(0), [6]uint64{^uint64(0)})
	if call.Args[0].Decoded || string(call.Args[0].Value) != "18446744073709551615" || !strings.HasPrefix(call.Name, "sys_") {
		t.Fatal("raw uint64 value was lost")
	}
}
