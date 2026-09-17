// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

// Package reflectxtype transfers dynamic reflectx type definitions. It requires
// Go 1.26.6 and -ldflags=-checklinkname=0. Static dependencies require the same
// executable and loaded type modules. Method definitions travel with types;
// callers transfer their functions separately and install the restored callbacks.
package reflectxtype

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"sync"
	"unsafe"

	"github.com/goplus/reflectx"
)

// Snapshot holds encoded definitions and their host-local, nonzero IDs. Only
// Data crosses the process boundary. IDs include referenced system types;
// system references use the same static locations as the reflecttype codec.
type Snapshot struct {
	Data    []byte
	IDs     map[reflect.Type]uint32
	Methods []reflect.Value
}

type ReflectType struct {
	types       []reflect.Type
	definitions []definition
	ctx         *reflectx.Context
	methodCount int
}

// Resolve returns a completed local type. Concurrent Resolve calls are safe.
func (t *ReflectType) Resolve(id uint32) (reflect.Type, error) {
	if id == 0 || uint64(id) > uint64(len(t.types)) {
		return nil, fmt.Errorf("unknown reflectx type ID %d", id)
	}
	return t.types[id-1], nil
}

// reflectx.StructOf updates descriptors obtained from standard reflect caches.
// Serialize our imports and exports; callers must also finish their own type
// construction and Context.Reset calls before starting a transfer.
var transferMu sync.Mutex

// Export reads reflectx.Default's type caches and standard reflect caches.
// A type created outside Default can be exposed with reflect.SliceOf(typ), as
// state already does for reflecttype. Cache enumeration is not a whole-process
// registry or an atomic snapshot of external constructor calls.
func Export() (*Snapshot, error) {
	if runtime.Version() != "go1.26.6" {
		return nil, fmt.Errorf("reflectxtype requires go1.26.6, got %s", runtime.Version())
	}
	transferMu.Lock()
	defer transferMu.Unlock()

	roots := cachedTypes()
	// The leading three fields match reflectx v1.7.8 Context. Retain typed
	// references, including both sides of the embedded-method lookup cache.
	ctx := (*struct {
		embed      map[reflect.Type]reflect.Type
		structs    map[string][]reflect.Type
		interfaces map[string]reflect.Type
	})(unsafe.Pointer(reflectx.Default))
	e := exporter{
		ids:      make(map[reflect.Type]uint32),
		needed:   make(map[reflect.Type]bool),
		visiting: make(map[reflect.Type]bool),
	}
	for from, to := range ctx.embed {
		roots = append(roots, from, to)
		e.needed[from], e.needed[to] = true, true
	}
	for _, bucket := range ctx.structs {
		for _, typ := range bucket {
			roots = append(roots, typ)
			e.needed[typ] = true
		}
	}
	for _, typ := range ctx.interfaces {
		roots = append(roots, typ)
		e.needed[typ] = true
	}
	for _, typ := range roots {
		if e.requiresReflectx(typ) {
			if _, err := e.intern(typ); err != nil {
				return nil, err
			}
		}
	}
	data := binary.AppendUvarint(nil, uint64(len(e.entries)))
	for _, entry := range e.entries {
		data = binary.AppendUvarint(data, uint64(len(entry)))
		data = append(data, entry...)
	}
	return &Snapshot{Data: data, IDs: e.ids, Methods: e.methods}, nil
}

// Zero denotes an executable type location, primitive kinds denote builtins,
// and dynamic|kind denotes a definition with identity and layout metadata.
const dynamic = 1 << 8
const concreteMethods = 1 << 9

type exporter struct {
	ids      map[reflect.Type]uint32
	entries  [][]byte
	needed   map[reflect.Type]bool
	visiting map[reflect.Type]bool
	methods  []reflect.Value
}

func (e *exporter) requiresReflectx(typ reflect.Type) bool {
	if builtinTypes[typ.Kind()] == typ {
		return false
	}
	if _, ok := staticTypes().byType[typ]; ok {
		return false
	}
	if need, ok := e.needed[typ]; ok {
		return need
	}
	if typ.Name() != "" || typ.Kind() == reflect.Interface || typ.Kind() == reflect.Struct || reflectx.NumMethodX(typ) != 0 || e.visiting[typ] {
		e.needed[typ] = true
		return true
	}
	e.visiting[typ] = true
	need := false
	for _, child := range appendDependencies(nil, typ) {
		if e.requiresReflectx(child) {
			need = true
		}
	}
	delete(e.visiting, typ)
	e.needed[typ] = need
	return need
}

func (e *exporter) intern(typ reflect.Type) (uint32, error) {
	if id, ok := e.ids[typ]; ok {
		return id, nil
	}
	if uint64(len(e.entries)) == math.MaxUint32 {
		return 0, fmt.Errorf("too many reflectx types")
	}
	id := uint32(len(e.entries)) + 1
	e.ids[typ] = id
	e.entries = append(e.entries, nil)
	data, err := e.encode(typ)
	if err != nil {
		return 0, fmt.Errorf("export reflectx type %v: %w", typ, err)
	}
	e.entries[id-1] = data
	return id, nil
}

func (e *exporter) encode(typ reflect.Type) ([]byte, error) {
	kind := typ.Kind()
	if builtinTypes[kind] == typ {
		return binary.AppendUvarint(nil, uint64(kind)), nil
	}
	if location, ok := staticTypes().byType[typ]; ok {
		data := binary.AppendUvarint(nil, 0)
		data = binary.AppendUvarint(data, uint64(location.module))
		return binary.AppendUvarint(data, location.offset), nil
	}
	var methods []reflectx.Method
	var functions []reflect.Value
	if kind != reflect.Interface && (kind != reflect.Pointer || typ.Name() != "") {
		methods, functions = concreteMethodSet(typ)
	}
	tag := dynamic | uint64(kind)
	if len(methods) != 0 {
		tag |= concreteMethods
	}
	data := binary.AppendUvarint(nil, tag)
	data = appendString(data, typ.Name())
	data = appendString(data, typ.PkgPath())
	data = binary.AppendUvarint(data, uint64(typ.Size()))
	data = binary.AppendUvarint(data, uint64(typ.Align()))
	data = appendFlag(data, typ.Comparable())
	appendType := func(child reflect.Type) error {
		id, err := e.intern(child)
		if err == nil {
			data = binary.AppendUvarint(data, uint64(id))
		}
		return err
	}
	switch kind {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		if err := appendType(typ.Elem()); err != nil {
			return nil, err
		}
		if kind == reflect.Array {
			data = binary.AppendUvarint(data, uint64(typ.Len()))
		} else if kind == reflect.Chan {
			data = binary.AppendUvarint(data, uint64(typ.ChanDir()))
		}
	case reflect.Map:
		if err := appendType(typ.Key()); err != nil {
			return nil, err
		}
		if err := appendType(typ.Elem()); err != nil {
			return nil, err
		}
	case reflect.Func:
		data = appendFlag(data, typ.IsVariadic())
		data = binary.AppendUvarint(data, uint64(typ.NumIn()))
		for i := 0; i < typ.NumIn(); i++ {
			if err := appendType(typ.In(i)); err != nil {
				return nil, err
			}
		}
		data = binary.AppendUvarint(data, uint64(typ.NumOut()))
		for i := 0; i < typ.NumOut(); i++ {
			if err := appendType(typ.Out(i)); err != nil {
				return nil, err
			}
		}
	case reflect.Struct:
		data = binary.AppendUvarint(data, uint64(typ.NumField()))
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			data = appendString(data, field.Name)
			data = appendString(data, field.PkgPath)
			data = appendString(data, string(field.Tag))
			data = appendFlag(data, field.Anonymous)
			data = binary.AppendUvarint(data, uint64(field.Offset))
			if err := appendType(field.Type); err != nil {
				return nil, err
			}
		}
	case reflect.Interface:
		data = binary.AppendUvarint(data, uint64(typ.NumMethod()))
		for i := 0; i < typ.NumMethod(); i++ {
			method := typ.Method(i)
			data = appendString(data, method.Name)
			data = appendString(data, method.PkgPath)
			if err := appendType(method.Type); err != nil {
				return nil, err
			}
		}
	default:
		if builtinTypes[kind] == nil {
			return nil, fmt.Errorf("unsupported kind %v", kind)
		}
	}
	if len(methods) != 0 {
		data = binary.AppendUvarint(data, uint64(len(methods)))
		for i, method := range methods {
			data = appendString(data, method.Name)
			data = appendString(data, method.PkgPath)
			data = appendFlag(data, method.Pointer)
			if err := appendType(method.Type); err != nil {
				return nil, err
			}
			e.methods = append(e.methods, functions[i])
			data = binary.AppendUvarint(data, uint64(len(e.methods)))
		}
	}
	return data, nil
}

func appendString(data []byte, value string) []byte {
	data = binary.AppendUvarint(data, uint64(len(value)))
	return append(data, value...)
}

func appendFlag(data []byte, value bool) []byte {
	if value {
		return append(data, 1)
	}
	return append(data, 0)
}
