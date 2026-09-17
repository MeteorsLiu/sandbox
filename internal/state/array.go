package state

import (
	"reflect"
	"unsafe"
)

const bulkArrayThreshold = 64

// Only numeric scalars can bypass recursive encoding. For example, []string
// still needs pointer relocation, and []atomic.Int64 needs its struct handling.
func isNumericArray(typ reflect.Type) bool {
	if typ.Kind() != reflect.Array {
		return false
	}
	switch typ.Elem().Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return true
	default:
		return false
	}
}

// arrayBytes borrows addressable numeric array storage until the codec copies
// it. Host and guest use the same executable, so scalar layouts are identical.
func arrayBytes(obj reflect.Value) []byte {
	return unsafe.Slice((*byte)(obj.Addr().UnsafePointer()), int(obj.Type().Size()))
}
