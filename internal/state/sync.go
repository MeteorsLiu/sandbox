package state

import (
	"reflect"
	"runtime"
	"strings"
	"sync"
)

// syncFields projects synchronization wrappers onto their values. In
// particular, atomic.Pointer[T].v must be viewed as *T so state relocates its
// target instead of saving an untyped address. Other sync primitives are empty
// records; sync.Map keeps its data path. The source data graph is quiescent.
func syncFields(obj reflect.Value) ([]reflect.Value, bool) {
	typ := obj.Type()
	pkg, name := typ.PkgPath(), typ.Name()
	if pkg == "sync" && typ != reflect.TypeFor[sync.Map]() || pkg == "internal/sync" && name == "Mutex" {
		return nil, true
	}
	if pkg != "sync/atomic" {
		return nil, false
	}
	pointer := strings.HasPrefix(name, "Pointer[")
	if !pointer {
		switch name {
		case "Bool", "Int32", "Int64", "Uint32", "Uint64", "Uintptr", "Value":
		default:
			return nil, false
		}
	}
	if runtime.Version() != "go1.26.6" {
		Failf("atomic snapshots require go1.26.6, got %s", runtime.Version())
	}
	if !obj.CanAddr() {
		value := reflect.New(typ).Elem()
		value.Set(obj)
		obj = value
	}
	value := obj.FieldByName("v")
	if pointer {
		// Go retains T in the leading [0]*T field of atomic.Pointer[T].
		pointerType := typ.Field(0).Type.Elem()
		value = reflect.NewAt(pointerType, value.Addr().UnsafePointer()).Elem()
	}
	return []reflect.Value{value}, true
}
