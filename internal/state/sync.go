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
// records; sync.Map uses its own entry codec. The source data graph is quiescent.
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

// encodeSyncMap saves logical entries: the hash trie's *node pointers hide
// larger entry/indirect allocations that reflection cannot traverse.
func (es *encodeState) encodeSyncMap(src *sync.Map, encoded *structValue) {
	entries := &mapValue{}
	encoded.Alloc(1)
	*encoded.Field(0) = entries
	src.Range(func(key, value any) bool {
		i := len(entries.Keys)
		entries.Keys = append(entries.Keys, nil)
		entries.Values = append(entries.Values, nil)
		es.encodeInterface(reflect.ValueOf(&key).Elem(), &entries.Keys[i])
		es.encodeInterface(reflect.ValueOf(&value).Elem(), &entries.Values[i])
		return true
	})
}

func (ds *decodeState) decodeSyncMap(ods *objectDecodeState, dst *sync.Map, encoded *structValue) {
	if encoded.TypeID != 0 || encoded.Fields() != 1 {
		Failf("invalid sync.Map record")
	}
	entries, ok := (*encoded.Field(0)).(*mapValue)
	if !ok {
		Failf("invalid sync.Map entries")
	}
	dst.Clear()
	for i := range entries.Keys {
		var key, value any
		ds.decodeObject(ods, reflect.ValueOf(&key).Elem(), entries.Keys[i])
		ds.decodeObject(ods, reflect.ValueOf(&value).Elem(), entries.Values[i])
		// References already have their final addresses; Store need not wait
		// for pointees. Waiting would deadlock two maps containing each other.
		dst.Store(key, value)
	}
}
