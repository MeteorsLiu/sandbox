// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

package reflecttype

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
	"runtime"
)

// Open reconstructs the types in Data from an exported Snapshot. It retains no
// references to data, which the caller may release after Open returns.
// Invalid encodings and types rejected by reflect constructors return errors.
func Open(data []byte) (result *ReflectType, err error) {
	if runtime.Version() != "go1.26.6" {
		return nil, fmt.Errorf("reflecttype requires go1.26.6, got %s", runtime.Version())
	}
	// The byte reader and reflect constructors both report invalid input by
	// panic. Keep that confined to this import boundary; never return a partial table.
	defer func() {
		if failure := recover(); failure != nil {
			result = nil
			err = fmt.Errorf("open reflect types: %v", failure)
		}
	}()
	r := typeReader(data)
	n := r.count()
	if uint64(n) > math.MaxUint32 {
		return nil, fmt.Errorf("too many reflected types")
	}
	d := importer{
		entries:   make([][]byte, n),
		types:     make([]reflect.Type, n),
		resolving: make([]bool, n),
	}
	for i := range d.entries {
		length := r.count()
		d.entries[i] = r[:length]
		r = r[length:]
	}
	if len(r) != 0 {
		return nil, fmt.Errorf("trailing reflected type table data")
	}
	for i := range d.entries {
		d.resolve(uint64(i) + 1)
	}
	return &ReflectType{types: d.types}, nil
}

type importer struct {
	entries   [][]byte
	types     []reflect.Type
	resolving []bool
}

func (d *importer) resolve(id uint64) reflect.Type {
	if id == 0 || id > uint64(len(d.entries)) {
		panic(fmt.Errorf("unknown reflected type ID %d", id))
	}
	index := id - 1
	if typ := d.types[index]; typ != nil {
		return typ
	}
	if d.resolving[index] {
		panic(fmt.Errorf("cyclic dynamic reflected type at ID %d", id))
	}
	d.resolving[index] = true
	r := typeReader(d.entries[index])
	kind := r.uint()
	var typ reflect.Type
	if kind < uint64(len(builtinTypes)) {
		typ = builtinTypes[kind]
	}
	if typ == nil {
		switch kind {
		case uint64(reflect.Invalid):
			module, offset := r.uint(), r.uint()
			if module > math.MaxUint32 {
				panic(fmt.Errorf("invalid static type module %d", module))
			}
			typ = staticTypes().byLocation[staticLocation{uint32(module), offset}]
			if typ == nil {
				panic(fmt.Errorf("static type module=%d offset=%#x is unavailable", module, offset))
			}
		case uint64(reflect.Pointer):
			typ = reflect.PointerTo(d.resolve(r.uint()))
		case uint64(reflect.Slice):
			typ = reflect.SliceOf(d.resolve(r.uint()))
		case uint64(reflect.Array):
			elem := d.resolve(r.uint())
			length := r.uint()
			if length > uint64(math.MaxInt) {
				panic(fmt.Errorf("array length %d overflows int", length))
			}
			typ = reflect.ArrayOf(int(length), elem)
		case uint64(reflect.Chan):
			elem := d.resolve(r.uint())
			direction := r.uint()
			if direction < uint64(reflect.RecvDir) || direction > uint64(reflect.BothDir) {
				panic(fmt.Errorf("invalid channel direction %d", direction))
			}
			typ = reflect.ChanOf(reflect.ChanDir(direction), elem)
		case uint64(reflect.Map):
			key := d.resolve(r.uint())
			typ = reflect.MapOf(key, d.resolve(r.uint()))
		case uint64(reflect.Func):
			variadic := r.flag()
			in := make([]reflect.Type, r.count())
			for i := range in {
				in[i] = d.resolve(r.uint())
			}
			out := make([]reflect.Type, r.count())
			for i := range out {
				out[i] = d.resolve(r.uint())
			}
			typ = reflect.FuncOf(in, out, variadic)
		case uint64(reflect.Struct):
			fields := make([]reflect.StructField, r.count())
			for i := range fields {
				fields[i] = reflect.StructField{
					Name: r.string(), PkgPath: r.string(), Tag: reflect.StructTag(r.string()),
					Anonymous: r.flag(), Type: d.resolve(r.uint()),
				}
			}
			typ = reflect.StructOf(fields)
		default:
			panic(fmt.Errorf("unsupported reflected kind %d", kind))
		}
	}
	if len(r) != 0 {
		panic(fmt.Errorf("trailing data for reflected type ID %d", id))
	}
	d.resolving[index] = false
	d.types[index] = typ
	return typ
}

type typeReader []byte

func (r *typeReader) uint() uint64 {
	value, n := binary.Uvarint(*r)
	if n == 0 {
		panic(io.ErrUnexpectedEOF)
	}
	if n < 0 {
		panic(fmt.Errorf("reflected type integer overflow"))
	}
	*r = (*r)[n:]
	return value
}

func (r *typeReader) count() int {
	n := r.uint()
	if n > uint64(len(*r)) {
		panic(io.ErrUnexpectedEOF)
	}
	return int(n)
}

func (r *typeReader) string() string {
	n := r.count()
	value := string((*r)[:n])
	*r = (*r)[n:]
	return value
}

func (r *typeReader) flag() bool {
	value := r.uint()
	if value > 1 {
		panic(fmt.Errorf("invalid reflected type flag %d", value))
	}
	return value == 1
}
