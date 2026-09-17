// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

package reflectxtype

import (
	"reflect"
	"slices"
	"strconv"
	"unsafe"

	"github.com/goplus/reflectx"
)

// These descriptor layouts match Go 1.26.6. Open checks the runtime version
// before constructing types. Pointer fields stay typed for the garbage collector.
type runtimeType struct {
	size, pointerBytes             uintptr
	hash                           uint32
	flags, align, fieldAlign, kind uint8
	equal                          func(unsafe.Pointer, unsafe.Pointer) bool
	gcData                         *byte
	stringOffset, pointerToThis    int32
}

type runtimeName struct{ bytes *byte }

type runtimeStructField struct {
	name   runtimeName
	typ    *runtimeType
	offset uintptr
}

type runtimeStructType struct {
	runtimeType
	pkgPath runtimeName
	fields  []runtimeStructField
}

//go:linkname reflectNewName reflect.newName
func reflectNewName(name, tag string, exported, embedded bool) runtimeName

func structOf(fields []reflect.StructField) reflect.Type {
	layout := slices.Clone(fields)
	var embedded bool
	var blanks int
	for i, field := range fields {
		if field.Anonymous {
			embedded = true
			layout[i].Anonymous = false
		} else if field.Name == "_" {
			if blanks != 0 {
				layout[i].Name = "_gop_underscore_" + strconv.Itoa(i)
			}
			blanks++
		}
	}
	base := reflect.StructOf(layout)
	if !embedded && blanks == 0 {
		return base
	}

	// reflectx.StructOf edits reflect's cached fields in place. A named clone
	// alone still shares Fields and Name.Bytes: copy both before changing them,
	// so restoring struct{ T } cannot alter an existing struct{ T T }.
	typ := reflectx.NamedTypeOf("", "", base)
	original := (*runtimeStructType)((*[2]unsafe.Pointer)(unsafe.Pointer(&base))[1])
	cloned := (*runtimeStructType)((*[2]unsafe.Pointer)(unsafe.Pointer(&typ))[1])
	cloned.pkgPath = original.pkgPath
	cloned.fields = slices.Clone(original.fields)
	for i, field := range fields {
		cloned.fields[i].name = reflectNewName(field.Name, string(field.Tag), field.IsExported(), field.Anonymous)
	}
	if cloned.equal != nil && blanks != 0 {
		cloned.equal = func(p, q unsafe.Pointer) bool {
			for i, field := range cloned.fields {
				if fields[i].Name == "_" {
					continue
				}
				if !field.typ.equal(unsafe.Add(p, field.offset), unsafe.Add(q, field.offset)) {
					return false
				}
			}
			return true
		}
	}
	return typ
}
