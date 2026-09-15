//go:build linux && (amd64 || arm64)

package main

//go:generate go run ./internal/stracegen

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"unicode/utf8"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/marshal"
	slinux "gvisor.dev/gvisor/pkg/sentry/syscalls/linux"
)

type syscallFormat struct {
	name string
	args []string
}

type decodedArgument struct {
	Format  string          `json:"format"`
	Value   json.RawMessage `json:"value"`
	Decoded bool            `json:"decoded"`
}

type decodedSyscall struct {
	Number uint64            `json:"number"`
	Name   string            `json:"name"`
	Args   []decodedArgument `json:"args"`
}

// pointerFixup locates an absolute guest pointer in a staged input mapping.
type pointerFixup struct{ at, target int }

type syscallMemory interface {
	read(uint64, []byte) (int, error)
	allocate([]byte, []pointerFixup) (uint64, error)
}

type syscallCodec struct {
	memory   syscallMemory
	decoded  *decodedSyscall
	original [6]uint64
}

// Bytes are base64 in JSON; this also preserves non-UTF-8 Linux pathnames.
type byteValue struct {
	Bytes []byte `json:"bytes"`
}

func stringValue(s string) any {
	if utf8.ValidString(s) {
		return s
	}
	return byteValue{Bytes: []byte(s)}
}

func parseString(data json.RawMessage) (string, error) {
	var s string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	var b byteValue
	if err := json.Unmarshal(data, &b); err != nil || b.Bytes == nil {
		return "", fmt.Errorf("expected a string or bytes object")
	}
	return string(b.Bytes), nil
}

func (c *syscallCodec) read(address uint64, size int, budget *int) ([]byte, error) {
	if size < 0 || size > *budget {
		return nil, fmt.Errorf("argument exceeds decode byte budget")
	}
	*budget -= size
	data := make([]byte, size)
	n, err := c.memory.read(address, data)
	if err != nil {
		return nil, err
	}
	if n != size {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}

func (c *syscallCodec) readString(address uint64, maxlen int, budget *int) (string, error) {
	var result []byte
	for len(result) < maxlen {
		size := min(256, maxlen-len(result), *budget)
		if size <= 0 {
			return "", fmt.Errorf("string exceeds decode byte budget")
		}
		buf := make([]byte, size)
		n, err := c.memory.read(address, buf)
		*budget -= n
		if end := bytes.IndexByte(buf[:n], 0); end >= 0 {
			return string(append(result, buf[:end]...)), nil
		}
		result = append(result, buf[:n]...)
		if err != nil {
			return "", err
		}
		if n == 0 || address+uint64(n) < address {
			return "", io.ErrUnexpectedEOF
		}
		address += uint64(n)
	}
	return "", fmt.Errorf("string has no terminator within %d bytes", maxlen)
}

func argumentStruct(kind string) marshal.Marshallable {
	switch kind {
	case "Timespec":
		return new(linux.Timespec)
	case "ItimerVal":
		return new(linux.ItimerVal)
	case "ItimerSpec":
		return new(linux.Itimerspec)
	case "OpenHow":
		return new(linux.OpenHow)
	case "SigSet":
		return new(linux.SignalSet)
	case "SigAction":
		return new(linux.SigAction)
	case "EpollEvent":
		return new(linux.EpollEvent)
	}
	return nil
}

func scalarFormat(kind string) bool {
	switch kind {
	case "Hex", "Oct", "FD", "SockFamily", "SockType", "SockProtocol", "SockFlags",
		"CloneFlags", "OpenFlags", "Mode", "FutexOp", "PtraceRequest", "ItimerType",
		"Signal", "SignalMaskAction", "SockOptLevel", "SockOptName", "EpollCtlOp",
		"MmapProt", "MmapFlags", "CloseRangeFlags":
		return true
	}
	return false
}

func (c *syscallCodec) decode(number uint64, args [6]uint64, budget int) ([]byte, error) {
	c.decoded = nil
	info, ok := syscallFormats[number]
	if !ok {
		info = syscallFormat{name: fmt.Sprintf("sys_%d", number), args: []string{"Hex", "Hex", "Hex", "Hex", "Hex", "Hex"}}
	}
	call := &decodedSyscall{Number: number, Name: info.name, Args: make([]decodedArgument, len(info.args))}
	for i, kind := range info.args {
		value, decoded := any(args[i]), false
		var err error
		if ok {
			value, decoded, err = c.decodeArgument(info.name, kind, i, args, &budget)
		}
		if err != nil {
			return nil, fmt.Errorf("%s argument %d (%s): %w", info.name, i, kind, err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		call.Args[i] = decodedArgument{Format: kind, Value: data, Decoded: decoded}
	}
	data, err := json.Marshal(call)
	if err == nil {
		c.decoded, c.original = call, args
	}
	return data, err
}

func (c *syscallCodec) decodeArgument(name, kind string, index int, args [6]uint64, budget *int) (any, bool, error) {
	address := args[index]
	// strace's display formats do not encode direction or every overloaded
	// layout. For example, ppoll writes its remaining timeout back to memory.
	switch name {
	case "ppoll":
		if kind == "Timespec" {
			return address, false, nil
		}
	case "pselect6":
		if kind == "Timespec" || kind == "SigSet" {
			return address, false, nil
		}
	case "listxattr", "llistxattr", "flistxattr":
		if index == 1 {
			return address, false, nil
		}
	case "mount":
		if index == 4 { // Filesystem-specific data may be binary.
			return address, false, nil
		}
	case "io_pgetevents", "io_uring_enter":
		if kind == "SigSet" { // May be a wrapper containing pointers and sizes.
			return address, false, nil
		}
	case "epoll_ctl":
		if kind == "EpollEvent" && args[1] == linux.EPOLL_CTL_DEL {
			return address, false, nil
		}
	case "futex":
		if kind == "Timespec" {
			op := uint32(args[1]) &^ uint32(linux.FUTEX_PRIVATE_FLAG|linux.FUTEX_CLOCK_REALTIME)
			if op != linux.FUTEX_WAIT && op != linux.FUTEX_WAIT_BITSET && op != linux.FUTEX_LOCK_PI {
				return address, false, nil
			}
		}
	}
	if kind == "FD" {
		return int32(address), true, nil
	}
	if scalarFormat(kind) {
		return address, true, nil
	}
	if object := argumentStruct(kind); object != nil {
		if address == 0 {
			return nil, true, nil
		}
		if kind == "OpenHow" && args[index+1] != uint64(object.SizeBytes()) {
			// Newer open_how layouts must retain unknown trailing bytes.
			return address, false, nil
		}
		data, err := c.read(address, object.SizeBytes(), budget)
		if err != nil {
			return nil, true, err
		}
		object.UnmarshalBytes(data)
		return object, true, nil
	}
	switch kind {
	case "UTimeTimespec":
		if address == 0 {
			return nil, true, nil
		}
		var times [2]linux.Timespec
		data, err := c.read(address, 2*times[0].SizeBytes(), budget)
		if err != nil {
			return nil, true, err
		}
		for i := range times {
			data = times[i].UnmarshalBytes(data)
		}
		return times, true, nil
	case "Path":
		if address == 0 {
			return nil, true, nil
		}
		s, err := c.readString(address, linux.PATH_MAX, budget)
		return stringValue(s), true, err
	case "ExecveStringVector":
		if address == 0 {
			return nil, true, nil
		}
		values := make([]any, 0)
		for {
			word, err := c.read(address, 8, budget)
			if err != nil {
				return nil, true, err
			}
			pointer := binary.LittleEndian.Uint64(word)
			if pointer == 0 {
				return values, true, nil
			}
			s, err := c.readString(pointer, slinux.ExecMaxElemSize, budget)
			if err != nil {
				return nil, true, err
			}
			values = append(values, stringValue(s))
			if address+8 < address {
				return nil, true, fmt.Errorf("vector address overflow")
			}
			address += 8
		}
	case "WriteBuffer", "SockAddr", "SetSockOptVal":
		length := args[index+1]
		if length > uint64(*budget) {
			return nil, true, fmt.Errorf("buffer exceeds decode byte budget")
		}
		data, err := c.read(address, int(length), budget)
		return byteValue{Bytes: data}, true, err
	default:
		// Output arguments have no result at entry. Complex formats without
		// an input codec remain explicitly raw, including nested msghdrs.
		return address, false, nil
	}
}

func sameJSON(a, b json.RawMessage) bool {
	var av, bv any
	ad, bd := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	ad.UseNumber()
	bd.UseNumber()
	return ad.Decode(&av) == nil && bd.Decode(&bv) == nil && reflect.DeepEqual(av, bv)
}

func (c *syscallCodec) rewrite(number uint64, args [6]uint64, data []byte) ([6]uint64, error) {
	if c.decoded == nil || c.decoded.Number != number || c.original != args {
		return args, fmt.Errorf("Decode must precede Rewrite with unchanged raw registers")
	}
	var call decodedSyscall
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&call); err != nil {
		return args, err
	}
	if call.Number != number || call.Name != c.decoded.Name || len(call.Args) != len(c.decoded.Args) {
		return args, fmt.Errorf("syscall identity and argument count cannot be changed")
	}
	out := args
	var payload []byte
	var fixups []pointerFixup
	offsets := make(map[int]int)
	lengths := make(map[int]uint64)
	for i, arg := range call.Args {
		original := c.decoded.Args[i]
		if arg.Format != original.Format || arg.Decoded != original.Decoded {
			return args, fmt.Errorf("argument %d classification cannot be changed", i)
		}
		if sameJSON(arg.Value, original.Value) {
			continue
		}
		if !arg.Decoded || scalarFormat(arg.Format) {
			var value json.Number
			if len(arg.Value) == 0 || arg.Value[0] == '"' || json.Unmarshal(arg.Value, &value) != nil {
				return args, fmt.Errorf("argument %d must be an integer", i)
			}
			var err error
			if arg.Format == "FD" {
				var signed int64
				signed, err = strconv.ParseInt(value.String(), 10, 32)
				out[i] = uint64(signed)
			} else {
				out[i], err = strconv.ParseUint(value.String(), 10, 64)
			}
			if err != nil {
				return args, fmt.Errorf("argument %d must be an integer: %w", i, err)
			}
			continue
		}
		if bytes.Equal(arg.Value, []byte("null")) {
			out[i] = 0
			continue
		}
		for len(payload)%8 != 0 {
			payload = append(payload, 0)
		}
		offset := len(payload)
		if object := argumentStruct(arg.Format); object != nil {
			decoder := json.NewDecoder(bytes.NewReader(arg.Value))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(object); err != nil {
				return args, err
			}
			payload = append(payload, make([]byte, object.SizeBytes())...)
			object.MarshalBytes(payload[offset:])
		} else {
			switch arg.Format {
			case "UTimeTimespec":
				var times []linux.Timespec
				decoder := json.NewDecoder(bytes.NewReader(arg.Value))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&times); err != nil || len(times) != 2 {
					return args, fmt.Errorf("utimensat requires two timespec values")
				}
				for j := range times {
					start := len(payload)
					payload = append(payload, make([]byte, times[j].SizeBytes())...)
					times[j].MarshalBytes(payload[start:])
				}
			case "Path":
				s, err := parseString(arg.Value)
				if err != nil || len(s) >= linux.PATH_MAX || bytes.IndexByte([]byte(s), 0) >= 0 {
					return args, fmt.Errorf("invalid replacement path at argument %d", i)
				}
				payload = append(append(payload, s...), 0)
			case "ExecveStringVector":
				var values []json.RawMessage
				if err := json.Unmarshal(arg.Value, &values); err != nil {
					return args, err
				}
				payload = append(payload, make([]byte, (len(values)+1)*8)...)
				for n, value := range values {
					s, err := parseString(value)
					if err != nil || len(s) >= slinux.ExecMaxElemSize || bytes.IndexByte([]byte(s), 0) >= 0 {
						return args, fmt.Errorf("invalid string vector element %d", n)
					}
					fixups = append(fixups, pointerFixup{at: offset + n*8, target: len(payload)})
					payload = append(append(payload, s...), 0)
				}
			case "WriteBuffer", "SockAddr", "SetSockOptVal":
				var value byteValue
				if err := json.Unmarshal(arg.Value, &value); err != nil || value.Bytes == nil {
					return args, fmt.Errorf("expected bytes at argument %d", i)
				}
				payload = append(payload, value.Bytes...)
				lengths[i+1] = uint64(len(value.Bytes))
			default:
				return args, fmt.Errorf("no encoder for %s", arg.Format)
			}
		}
		offsets[i] = offset
	}
	for index, length := range lengths {
		if !sameJSON(call.Args[index].Value, c.decoded.Args[index].Value) && out[index] != length {
			return args, fmt.Errorf("argument %d conflicts with edited buffer length", index)
		}
		out[index] = length
	}
	if len(offsets) != 0 {
		if len(payload) == 0 {
			payload = append(payload, 0)
		}
		base, err := c.memory.allocate(payload, fixups)
		if err != nil {
			return args, err
		}
		for index, offset := range offsets {
			out[index] = base + uint64(offset)
		}
	}
	// A second rewrite must start from a new snapshot of the updated pointers.
	c.decoded = nil
	return out, nil
}
