package state

import (
	"fmt"
	"reflect"
	"strings"
	"unsafe"
)

func nativeTypeDependencies(typ reflect.Type) []reflect.Type {
	var result []reflect.Type
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		result = append(result, typ.Elem())
	case reflect.Map:
		result = append(result, typ.Key(), typ.Elem())
	case reflect.Func:
		for i := 0; i < typ.NumIn(); i++ {
			result = append(result, typ.In(i))
		}
		for i := 0; i < typ.NumOut(); i++ {
			result = append(result, typ.Out(i))
		}
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			result = append(result, typ.Field(i).Type)
		}
	case reflect.Interface:
		for i := 0; i < typ.NumMethod(); i++ {
			result = append(result, typ.Method(i).Type)
		}
	}
	return result
}

// nativeShapeMatches compares a compiler shape with a concrete type. Pointer
// shapes erase their pointee: go.shape.*uint8 can represent *Node, not a byte
// inside Node. Other composite shapes retain their fields and element shapes.
func nativeShapeMatches(shape, concrete reflect.Type) bool {
	seen := make(map[[2]reflect.Type]bool)
	var match func(reflect.Type, reflect.Type) bool
	match = func(a, b reflect.Type) bool {
		if a == b {
			return true
		}
		if a.Kind() != b.Kind() || a.Size() != b.Size() || a.Align() != b.Align() {
			return false
		}
		shaped := a.PkgPath() == "go.shape" || strings.Contains(a.Name(), "go.shape.")
		if a.Name() != "" && !shaped {
			return false
		}
		if a.Name() != "" && a.PkgPath() != "go.shape" {
			// Box[shape] must resolve to Box[T], not another named type
			// with the same fields that happens to occur in the dictionary.
			origin, _, _ := strings.Cut(a.Name(), "[")
			other, _, _ := strings.Cut(b.Name(), "[")
			if a.PkgPath() != b.PkgPath() || origin != other {
				return false
			}
		}
		if a.PkgPath() == "go.shape" && a.Kind() == reflect.Pointer {
			return true
		}
		key := [2]reflect.Type{a, b}
		if seen[key] {
			return true
		}
		seen[key] = true
		switch a.Kind() {
		case reflect.Pointer, reflect.Slice:
			return match(a.Elem(), b.Elem())
		case reflect.Array:
			return a.Len() == b.Len() && match(a.Elem(), b.Elem())
		case reflect.Chan:
			return a.ChanDir() == b.ChanDir() && match(a.Elem(), b.Elem())
		case reflect.Map:
			return match(a.Key(), b.Key()) && match(a.Elem(), b.Elem())
		case reflect.Func:
			if a.NumIn() != b.NumIn() || a.NumOut() != b.NumOut() || a.IsVariadic() != b.IsVariadic() {
				return false
			}
			for i := 0; i < a.NumIn(); i++ {
				if !match(a.In(i), b.In(i)) {
					return false
				}
			}
			for i := 0; i < a.NumOut(); i++ {
				if !match(a.Out(i), b.Out(i)) {
					return false
				}
			}
		case reflect.Struct:
			if a.NumField() != b.NumField() {
				return false
			}
			for i := 0; i < a.NumField(); i++ {
				af, bf := a.Field(i), b.Field(i)
				if af.Name != bf.Name || af.PkgPath != bf.PkgPath || af.Offset != bf.Offset || af.Anonymous != bf.Anonymous || af.Tag != bf.Tag || !match(af.Type, bf.Type) {
					return false
				}
			}
		case reflect.Interface:
			if a.NumMethod() != b.NumMethod() {
				return false
			}
			for i := 0; i < a.NumMethod(); i++ {
				am, bm := a.Method(i), b.Method(i)
				if am.Name != bm.Name || am.PkgPath != bm.PkgPath || !match(am.Type, bm.Type) {
					return false
				}
			}
		default:
			return shaped
		}
		return true
	}
	return match(shape, concrete)
}

// environmentView gives the graph codec concrete capture types without
// changing the funcval's bytes. Dictionary slots become uintptr references to
// validated, immutable ELF data: copying their arrays would register read-only
// host storage for writeback. All heap pointers retain pointer-typed slots.
func (m *nativeMetadata) environmentView(storage reflect.Value) (reflect.Value, error) {
	typ := storage.Type()
	fields := make([]reflect.StructField, typ.NumField())
	type dictionary struct{ address, size uintptr }
	var dictionaries []dictionary
	for i := range fields {
		fields[i] = typ.Field(i)
		ft := fields[i].Type
		if ft.Kind() != reflect.Pointer || ft.Elem().Kind() != reflect.Array || ft.Elem().Elem() != reflect.TypeFor[uintptr]() {
			continue
		}
		address := storage.Field(i).Pointer()
		if size, ok := m.dictionaries[address]; ok && ft.Elem().Size() <= size {
			dictionaries = append(dictionaries, dictionary{address, ft.Elem().Size()})
			fields[i].Type = reflect.TypeFor[uintptr]()
		}
	}
	if len(dictionaries) == 0 && !strings.Contains(typ.String(), "go.shape.") {
		return storage, nil
	}
	// Runtime dictionaries may refer to each other. Follow only known symbols,
	// and use the static type index to distinguish rtypes from PCs and itabs.
	candidates := make(map[reflect.Type]bool)
	visited := make(map[dictionary]bool)
	for i := 0; i < len(dictionaries); i++ {
		dict := dictionaries[i]
		if visited[dict] {
			continue
		}
		visited[dict] = true
		for offset := uintptr(0); offset < dict.size; offset += 8 {
			word := *(*uintptr)(unsafe.Pointer(dict.address + offset))
			if candidate := m.staticTypes[word]; candidate != nil {
				candidates[candidate] = true
			} else if size, ok := m.dictionaries[word]; ok {
				dictionaries = append(dictionaries, dictionary{word, size})
			}
		}
	}
	hasShape := make(map[reflect.Type]bool)
	var shaped func(reflect.Type) bool
	shaped = func(t reflect.Type) bool {
		if found, ok := hasShape[t]; ok {
			return found
		}
		hasShape[t] = false
		if t.PkgPath() == "go.shape" || strings.Contains(t.Name(), "go.shape.") {
			hasShape[t] = true
			return true
		}
		for _, child := range nativeTypeDependencies(t) {
			if shaped(child) {
				hasShape[t] = true
				return true
			}
		}
		return false
	}
	var concrete func(reflect.Type) (reflect.Type, error)
	concrete = func(t reflect.Type) (reflect.Type, error) {
		if !shaped(t) {
			return t, nil
		}
		var found reflect.Type
		for candidate := range candidates {
			if nativeShapeMatches(t, candidate) {
				if found != nil && found != candidate {
					return nil, fmt.Errorf("ambiguous dictionary types for %v: %v and %v", t, found, candidate)
				}
				found = candidate
			}
		}
		if found != nil {
			return found, nil
		}
		// Closure captures add pointer indirection for variables held by
		// reference, e.g. OnceValue's *struct{... result T}. The dictionary
		// contains the concrete struct descriptor, not necessarily *struct.
		if t.Name() == "" && t.Kind() == reflect.Pointer {
			elem, err := concrete(t.Elem())
			if err != nil {
				return nil, err
			}
			return reflect.PointerTo(elem), nil
		}
		return nil, fmt.Errorf("dictionary has no concrete type for %v", t)
	}
	changed := false
	for i := range fields {
		var err error
		fields[i].Type, err = concrete(fields[i].Type)
		if err != nil {
			return reflect.Value{}, err
		}
		changed = changed || fields[i].Type != typ.Field(i).Type
	}
	if !changed {
		return storage, nil
	}
	view := reflect.StructOf(fields)
	if view.Size() != typ.Size() || view.Align() != typ.Align() {
		return reflect.Value{}, fmt.Errorf("generic closure view changes storage layout")
	}
	for i, field := range fields {
		if view.Field(i).Offset != field.Offset {
			return reflect.Value{}, fmt.Errorf("generic closure view changes capture offset")
		}
	}
	return reflect.NewAt(view, storage.Addr().UnsafePointer()).Elem(), nil
}

// validEnvironmentView checks the allocation layout before decoding any data.
// Once all objects are restored, environmentView verifies the concrete types
// against the actual dictionary. Only static dictionary slots may lose GC bits.
func validEnvironmentView(shape, view reflect.Type) bool {
	if view.Kind() != reflect.Struct || view.NumField() != shape.NumField() || view.Size() != shape.Size() || view.Align() != shape.Align() {
		return false
	}
	for i := 0; i < shape.NumField(); i++ {
		a, b := shape.Field(i), view.Field(i)
		if a.Name != b.Name || a.PkgPath != b.PkgPath || a.Offset != b.Offset || a.Tag != b.Tag || a.Anonymous != b.Anonymous {
			return false
		}
		if a.Type.Kind() == reflect.Pointer && a.Type.Elem().Kind() == reflect.Array && a.Type.Elem().Elem() == reflect.TypeFor[uintptr]() && b.Type == reflect.TypeFor[uintptr]() {
			continue
		}
		if !nativeShapeMatches(a.Type, b.Type) {
			return false
		}
	}
	return true
}
