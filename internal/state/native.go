package state

import (
	"reflect"
	"runtime"
	"unsafe"
)

//go:linkname nativeReflectType reflect.toType
func nativeReflectType(unsafe.Pointer) reflect.Type

type nativeState struct {
	storage map[reflect.Type]uintptr
}

func (ns *nativeState) layout(pc uintptr) reflect.Type {
	m, err := executableNativeMetadata()
	if err != nil {
		Failf("native closure metadata: %w", err)
	}
	typ, err := m.layout(pc)
	if err != nil {
		Failf("native closure layout: %w", err)
	}
	if ns.storage == nil {
		ns.storage = make(map[reflect.Type]uintptr)
	}
	ns.storage[typ] = pc
	return typ
}

func (es *encodeState) encodeFunction(obj reflect.Value, dest *object) {
	f := &functionValue{}
	*dest = f
	if obj.IsNil() {
		return
	}
	pc := obj.Pointer()
	typ := es.native.layout(pc)
	if !obj.CanAddr() {
		v := reflect.New(obj.Type()).Elem()
		v.Set(obj)
		obj = v
	}
	storage := *(*unsafe.Pointer)(obj.Addr().UnsafePointer())
	f.PC = uintValue(pc)
	es.resolve(reflect.NewAt(typ, storage), &f.Env)
	runtime.KeepAlive(obj)
}

type decodedFunction struct {
	pc      uintptr
	storage reflect.Value
}

func (ds *decodeState) decodeFunction(obj reflect.Value, f *functionValue) {
	if obj.Kind() != reflect.Func {
		Failf("function record cannot be decoded into %v", obj.Type())
	}
	if f.PC == 0 && f.Env.Root == 0 {
		obj.SetZero()
		return
	}
	if f.PC == 0 || f.Env.Root == 0 || len(f.Env.Dots) != 0 {
		Failf("invalid closure PC or environment reference")
	}
	typ := ds.native.layout(uintptr(f.PC))
	storage := ds.register(&f.Env, typ)
	if storage.Type() != typ {
		Failf("closure environment has type %v, want %v", storage.Type(), typ)
	}
	// The typed allocation keeps capture pointers visible to the local Go GC.
	// No allocator metadata or source heap addresses are imported.
	*(*unsafe.Pointer)(obj.Addr().UnsafePointer()) = storage.Addr().UnsafePointer()
	ds.functions = append(ds.functions, decodedFunction{uintptr(f.PC), storage})
}
