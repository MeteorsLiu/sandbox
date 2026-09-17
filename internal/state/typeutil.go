package state

import (
	"go/types"
	"reflect"
)

// typeutilMap is shared by the x/tools and gogen type maps. Their bucket
// layouts also match, so state can preserve shared tables while rehashing.
type typeutilMap interface {
	Iterate(func(types.Type, any))
	Set(types.Type, any) any
}

func isTypeutilMap(typ reflect.Type) bool {
	if !reflect.PointerTo(typ).Implements(reflect.TypeFor[typeutilMap]()) {
		return false
	}
	table, ok := typ.FieldByName("table")
	if !ok || len(table.Index) != 1 || table.Type.Kind() != reflect.Map || table.Type.Key() != reflect.TypeFor[uint32]() || table.Type.Elem().Kind() != reflect.Slice {
		return false
	}
	entry := table.Type.Elem().Elem()
	return entry.Kind() == reflect.Struct && entry.NumField() == 2 &&
		entry.Field(0).Name == "key" && entry.Field(0).Type == reflect.TypeFor[types.Type]() &&
		entry.Field(1).Name == "value" && entry.Field(1).Type == reflect.TypeFor[any]()
}

// rehashTypeutilMap runs after key objects are populated: hashing a signature,
// for example, reads its parameter types, not just its address. Named types
// additionally depend on relocated pointers and the destination's hash seed.
func rehashTypeutilMap(m typeutilMap) {
	obj := reflect.ValueOf(m).Elem()
	rebuilt := reflect.New(obj.Type()).Interface().(typeutilMap)
	m.Iterate(func(key types.Type, value any) {
		rebuilt.Set(key, value)
	})

	// Keep the decoded table itself, since copies of a type map share it.
	// Replacing *m would leave copies held in interfaces or maps using old hashes.
	table := reflectValueRWAddr(obj.FieldByName("table")).Elem()
	buckets := reflectValueRWAddr(reflect.ValueOf(rebuilt).Elem().FieldByName("table")).Elem()
	table.Clear()
	for it := buckets.MapRange(); it.Next(); {
		table.SetMapIndex(it.Key(), it.Value())
	}
}
