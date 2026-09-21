// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

// Package reflecttype transfers types discovered in Go's reflect caches.
// It requires Go 1.26.6 and -ldflags=-checklinkname=0. Static types require
// the same executable and loaded type modules on both ends.
package reflecttype

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"runtime"
)

// Snapshot contains an encoded type table and its host-side type IDs.
// Only Data is transferred to the guest; IDs holds local reflect.Type values.
// IDs are nonzero and valid only for this snapshot.
type Snapshot struct {
	Data []byte
	IDs  map[reflect.Type]uint32
}

// Export discovers supported cached types and encodes their dependencies.
// Dynamic named types and interfaces, and composites depending on them, belong
// to a separate type provider and are omitted from this snapshot.
// Finish creating the types to be transferred before calling Export: cache enumeration is not
// an atomic snapshot, and types created afterward need a new export.
// Each call owns its IDs and data. Standard constructors check that cached
// dynamic descriptors can be reproduced without an extended type provider.
func Export() (*Snapshot, error) {
	if runtime.Version() != "go1.26.6" {
		return nil, fmt.Errorf("reflecttype requires go1.26.6, got %s", runtime.Version())
	}
	e := exporter{ids: make(map[reflect.Type]uint32), supported: make(map[reflect.Type]bool), static: indexStaticTypes().byType}
	for _, typ := range cachedTypes() {
		if !e.supports(typ) {
			continue
		}
		if _, err := e.intern(typ); err != nil {
			return nil, err
		}
	}
	data := binary.AppendUvarint(nil, uint64(len(e.entries)))
	for _, entry := range e.entries {
		data = binary.AppendUvarint(data, uint64(len(entry)))
		data = append(data, entry...)
	}
	return &Snapshot{Data: data, IDs: e.ids}, nil
}

// ReflectType contains guest-local types reconstructed by Open. It is immutable
// after Open returns, so concurrent calls to Resolve are safe.
type ReflectType struct {
	types []reflect.Type
}

// Resolve returns the local type for a nonzero ID from the exported snapshot.
func (t *ReflectType) Resolve(id uint32) (reflect.Type, error) {
	if id == 0 || uint64(id) > uint64(len(t.types)) {
		return nil, fmt.Errorf("unknown reflected type ID %d", id)
	}
	return t.types[id-1], nil
}

// Entry tags use reflect.Kind. Invalid (zero) identifies a static type location;
// primitive kinds need no payload. Composite payloads contain dependency IDs.
// Function types carry signatures only, never function PCs or environments.
type exporter struct {
	ids       map[reflect.Type]uint32
	entries   [][]byte
	supported map[reflect.Type]bool
	static    map[reflect.Type]staticLocation
}

func (e *exporter) supports(typ reflect.Type) (supported bool) {
	if builtinTypes[typ.Kind()] == typ {
		return true
	}
	if _, ok := e.static[typ]; ok {
		return true
	}
	if supported, ok := e.supported[typ]; ok {
		return supported
	}
	// A cycle outside the executable requires a dynamic named identity. Reserve
	// false before visiting children so it cannot become an infinite traversal.
	e.supported[typ] = false
	if typ.Name() != "" || typ.Kind() == reflect.Interface {
		return false
	}
	for _, child := range appendDependencies(nil, typ) {
		if !e.supports(child) {
			return false
		}
	}
	// For example, reflectx can attach private methods to an unnamed struct,
	// or create private embedded fields that reflect.StructOf rejects. Merely
	// checking Name and exported methods would silently discard that metadata.
	defer func() {
		if recover() != nil {
			supported = false
		}
		e.supported[typ] = supported
	}()
	var rebuilt reflect.Type
	switch typ.Kind() {
	case reflect.Pointer:
		rebuilt = reflect.PointerTo(typ.Elem())
	case reflect.Slice:
		rebuilt = reflect.SliceOf(typ.Elem())
	case reflect.Array:
		rebuilt = reflect.ArrayOf(typ.Len(), typ.Elem())
	case reflect.Chan:
		rebuilt = reflect.ChanOf(typ.ChanDir(), typ.Elem())
	case reflect.Map:
		rebuilt = reflect.MapOf(typ.Key(), typ.Elem())
	case reflect.Func:
		in, out := make([]reflect.Type, typ.NumIn()), make([]reflect.Type, typ.NumOut())
		for i := range in {
			in[i] = typ.In(i)
		}
		for i := range out {
			out[i] = typ.Out(i)
		}
		rebuilt = reflect.FuncOf(in, out, typ.IsVariadic())
	case reflect.Struct:
		fields := make([]reflect.StructField, typ.NumField())
		for i := range fields {
			fields[i] = typ.Field(i)
		}
		rebuilt = reflect.StructOf(fields)
	}
	return rebuilt == typ
}

func (e *exporter) intern(typ reflect.Type) (uint32, error) {
	if id, ok := e.ids[typ]; ok {
		return id, nil
	}
	if uint64(len(e.entries)) == math.MaxUint32 {
		return 0, fmt.Errorf("too many reflected types")
	}
	id := uint32(len(e.entries)) + 1
	e.ids[typ] = id
	e.entries = append(e.entries, nil)
	entry, err := e.encode(typ)
	if err != nil {
		return 0, err
	}
	e.entries[id-1] = entry
	return id, nil
}

func (e *exporter) encode(typ reflect.Type) ([]byte, error) {
	kind := typ.Kind()
	if builtinTypes[kind] == typ {
		return binary.AppendUvarint(nil, uint64(kind)), nil
	}
	if location, ok := e.static[typ]; ok {
		// A static entry is restored by location, but callers can also refer
		// to its dependencies directly (e.g. reflect.Type inside []reflect.Type).
		for _, dependency := range appendDependencies(nil, typ) {
			if _, err := e.intern(dependency); err != nil {
				return nil, err
			}
		}
		data := binary.AppendUvarint(nil, uint64(reflect.Invalid))
		data = binary.AppendUvarint(data, uint64(location.module))
		return binary.AppendUvarint(data, location.offset), nil
	}
	if typ.Name() != "" {
		return nil, fmt.Errorf("non-static named type %q is not reconstructible", typ)
	}
	data := binary.AppendUvarint(nil, uint64(kind))
	appendType := func(typ reflect.Type) error {
		id, err := e.intern(typ)
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
			if err := appendType(field.Type); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("non-static type %v is not reconstructible", typ)
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
