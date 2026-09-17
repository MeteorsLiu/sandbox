// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

package reflectxtype

import (
	"fmt"
	"go/token"
	"reflect"
	"unsafe"

	"github.com/goplus/reflectx"
)

// MethodX in reflectx v1.7.8 omits private method package paths. Read only
// that missing metadata; signatures, functions and installation use its API.
type runtimeMethod struct {
	name, signature, ifn, tfn int32
}

//go:linkname runtimeMethods github.com/goplus/reflectx.rtypeMethods
func runtimeMethods(unsafe.Pointer) []runtimeMethod

//go:linkname resolveMethodName reflect.resolveNameOff
func resolveMethodName(unsafe.Pointer, int32) unsafe.Pointer

//go:linkname methodPackage reflect.pkgPath
func methodPackage(struct{ bytes *byte }) string

func concreteMethodSet(typ reflect.Type) ([]reflectx.Method, []reflect.Value) {
	var methods []reflectx.Method
	var functions []reflect.Value
	type identity struct{ name, pkg string }
	values := make(map[identity]bool)
	for _, receiver := range []reflect.Type{typ, reflectx.PtrTo(typ)} {
		pointer := receiver != typ
		for i := 0; i < reflectx.NumMethodX(receiver); i++ {
			method := reflectx.MethodX(receiver, i)
			pkg := method.PkgPath
			if !token.IsExported(method.Name) {
				rt := (*[2]unsafe.Pointer)(unsafe.Pointer(&receiver))[1]
				name := resolveMethodName(rt, runtimeMethods(rt)[i].name)
				pkg = methodPackage(struct{ bytes *byte }{(*byte)(name)})
			}
			key := identity{method.Name, pkg}
			if pointer && values[key] {
				continue
			}
			values[key] = true
			in, out := make([]reflect.Type, method.Type.NumIn()-1), make([]reflect.Type, method.Type.NumOut())
			for j := range in {
				in[j] = method.Type.In(j + 1)
			}
			for j := range out {
				out[j] = method.Type.Out(j)
			}
			methods = append(methods, reflectx.Method{Name: method.Name, PkgPath: pkg, Pointer: pointer, Type: reflect.FuncOf(in, out, method.Type.IsVariadic())})
			functions = append(functions, method.Func)
		}
	}
	return methods, functions
}

// MethodCount is the number of callbacks required by SetMethods, in the same
// order as Snapshot.Methods. Types are not callable until these are installed.
func (t *ReflectType) MethodCount() int { return t.methodCount }

// SetMethods installs callbacks whose environments may still be undergoing
// graph restoration. No callback may run until the caller finishes that graph.
func (t *ReflectType) SetMethods(callbacks []func([]reflect.Value) []reflect.Value) (err error) {
	if len(callbacks) != t.methodCount {
		return fmt.Errorf("reflectx method callbacks: got %d, want %d", len(callbacks), t.methodCount)
	}
	transferMu.Lock()
	defer transferMu.Unlock()
	defer func() {
		if failure := recover(); failure != nil {
			err = fmt.Errorf("install reflectx methods: %v", failure)
		}
	}()
	for i, def := range t.definitions {
		if def.kind == reflect.Interface || len(def.methods) == 0 {
			continue
		}
		methods := make([]reflectx.Method, len(def.methods))
		for j, method := range def.methods {
			methods[j] = reflectx.Method{Name: method.name, PkgPath: method.pkg, Pointer: method.pointer, Type: t.types[method.typ-1], Func: callbacks[method.function-1]}
		}
		if err := t.ctx.SetMethodSet(t.types[i], methods, false); err != nil {
			return err
		}
	}
	return nil
}
