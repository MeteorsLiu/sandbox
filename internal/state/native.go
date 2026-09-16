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

var makeFuncPC = reflect.MakeFunc(reflect.TypeFor[func()](), nil).Pointer()

// Go 1.26.6 reflect.makeFuncImpl on linux/amd64 and linux/arm64. Both
// architectures use a two-byte abi.IntArgRegBitmap. Only fn is transferred;
// reflect.MakeFunc reconstructs the other fields in the receiving runtime.
type makeFuncImpl struct {
	code    uintptr
	stack   unsafe.Pointer
	argLen  uintptr
	regPtrs [2]byte
	ftyp    unsafe.Pointer
	fn      func([]reflect.Value) []reflect.Value
}

func makeFuncCallback(obj reflect.Value) reflect.Value {
	if _, err := executableNativeMetadata(); err != nil {
		Failf("MakeFunc metadata: %w", err)
	}
	if !obj.CanAddr() {
		v := reflect.New(obj.Type()).Elem()
		v.Set(obj)
		obj = v
	}
	impl := *(**makeFuncImpl)(obj.Addr().UnsafePointer())
	return reflect.ValueOf(&impl.fn).Elem()
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
	if pc == makeFuncPC {
		f.PC = uintValue(pc)
		es.resolve(makeFuncCallback(obj).Addr(), &f.Env)
		runtime.KeepAlive(obj)
		return
	}
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
	if uintptr(f.PC) == makeFuncPC {
		typ := reflect.TypeFor[func([]reflect.Value) []reflect.Value]()
		callback := ds.register(&f.Env, typ)
		if callback.Type() != typ {
			Failf("MakeFunc callback has type %v, want %v", callback.Type(), typ)
		}
		fn, ok := ds.makeFuncs[callback]
		if !ok {
			// The callback may refer back to this function. Publish the
			// wrapper now and install callbacks after the graph is decoded.
			fn = reflect.MakeFunc(obj.Type(), nil)
			if ds.makeFuncs == nil {
				ds.makeFuncs = make(map[reflect.Value]reflect.Value)
			}
			ds.makeFuncs[callback] = fn
		}
		if !fn.Type().ConvertibleTo(obj.Type()) {
			Failf("MakeFunc reference changes signature from %v to %v", fn.Type(), obj.Type())
		}
		obj.Set(fn.Convert(obj.Type()))
		return
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
