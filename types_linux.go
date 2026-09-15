//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"fmt"
	"reflect"
)

const (
	dynamicTypeBit = uintptr(1) << 63
	reflectTypeTag = ^uintptr(0)
)

// A program key names a type in one rebuilt interpreter. Other entries describe
// unnamed composites, such as the **Node cell created when Node is captured.
type transferType struct {
	Program int
	Key     string
	Kind    reflect.Kind
	Elem    uintptr
	MapKey  uintptr
	Length  int
	Dir     reflect.ChanDir
	In      []uintptr
	Out     []uintptr
	Vararg  bool
	Fields  []transferField
}

type transferField struct {
	Name, Package, Tag string
	Type               uintptr
	Anonymous          bool
}

func (im *valueImage) typeID(t reflect.Type) (uintptr, error) {
	if id, ok := im.typeIDs[t]; ok {
		return id, nil
	}
	d := transferType{Program: -1, Kind: t.Kind()}
	for i, p := range im.programs {
		if key, ok := p.typeKeys[t]; ok {
			d.Program, d.Key = i, key
			break
		}
	}
	if d.Program < 0 && (t.Name() != "" || t.NumMethod() != 0) {
		return 0, fmt.Errorf("type %s is absent from the ELF and rebuilt ixgo programs", t)
	}
	id := dynamicTypeBit | uintptr(len(im.typeDefs)+1)
	im.typeIDs[t] = id
	im.types[id] = t
	im.typeDefs = append(im.typeDefs, d)
	if d.Program >= 0 {
		return id, nil
	}
	var err error
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		d.Elem, err = im.typeID(t.Elem())
		if t.Kind() == reflect.Array {
			d.Length = t.Len()
		}
		if t.Kind() == reflect.Chan {
			d.Dir = t.ChanDir()
		}
	case reflect.Map:
		d.MapKey, err = im.typeID(t.Key())
		if err == nil {
			d.Elem, err = im.typeID(t.Elem())
		}
	case reflect.Func:
		d.Vararg = t.IsVariadic()
		for i := 0; i < t.NumIn() && err == nil; i++ {
			var id uintptr
			id, err = im.typeID(t.In(i))
			d.In = append(d.In, id)
		}
		for i := 0; i < t.NumOut() && err == nil; i++ {
			var id uintptr
			id, err = im.typeID(t.Out(i))
			d.Out = append(d.Out, id)
		}
	case reflect.Struct:
		for i := 0; i < t.NumField() && err == nil; i++ {
			f := t.Field(i)
			var id uintptr
			id, err = im.typeID(f.Type)
			d.Fields = append(d.Fields, transferField{f.Name, f.PkgPath, string(f.Tag), id, f.Anonymous})
		}
	default:
		err = fmt.Errorf("unsupported generated type %s", t)
	}
	im.typeDefs[int(id&^dynamicTypeBit)-1] = d
	return id, err
}

func (im *valueImage) resolveTypes() (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("invalid transfer type: %v", v)
		}
	}()
	active := make(map[uintptr]bool)
	var resolve func(uintptr) (reflect.Type, error)
	resolve = func(id uintptr) (reflect.Type, error) {
		if t, ok := im.types[id]; ok {
			return t, nil
		}
		i := int(id&^dynamicTypeBit) - 1
		if id&dynamicTypeBit == 0 || i < 0 || i >= len(im.typeDefs) || active[id] || len(active) >= 256 {
			return nil, fmt.Errorf("invalid type reference %#x", id)
		}
		active[id] = true
		defer delete(active, id)
		d := im.typeDefs[i]
		var t reflect.Type
		var err error
		if d.Program >= 0 {
			if d.Program >= len(im.programs) {
				return nil, fmt.Errorf("invalid type program %d", d.Program)
			}
			t = im.programs[d.Program].types[d.Key]
			if t == nil || t.Kind() != d.Kind {
				return nil, fmt.Errorf("ixgo type %q was not rebuilt", d.Key)
			}
		} else {
			switch d.Kind {
			case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan, reflect.Map:
				var elem reflect.Type
				elem, err = resolve(d.Elem)
				if err != nil {
					return nil, err
				}
				switch d.Kind {
				case reflect.Pointer:
					t = reflect.PointerTo(elem)
				case reflect.Slice:
					t = reflect.SliceOf(elem)
				case reflect.Array:
					if d.Length < 0 || uintptr(d.Length) > uintptr(len(im.mem)) {
						return nil, fmt.Errorf("invalid array length")
					}
					t = reflect.ArrayOf(d.Length, elem)
				case reflect.Chan:
					t = reflect.ChanOf(d.Dir, elem)
				case reflect.Map:
					key, e := resolve(d.MapKey)
					if e != nil {
						return nil, e
					}
					t = reflect.MapOf(key, elem)
				}
			case reflect.Func:
				in, out := make([]reflect.Type, len(d.In)), make([]reflect.Type, len(d.Out))
				for j, id := range d.In {
					in[j], err = resolve(id)
					if err != nil {
						return nil, err
					}
				}
				for j, id := range d.Out {
					out[j], err = resolve(id)
					if err != nil {
						return nil, err
					}
				}
				t = reflect.FuncOf(in, out, d.Vararg)
			case reflect.Struct:
				fields := make([]reflect.StructField, len(d.Fields))
				for j, f := range d.Fields {
					ft, e := resolve(f.Type)
					if e != nil {
						return nil, e
					}
					fields[j] = reflect.StructField{Name: f.Name, PkgPath: f.Package, Type: ft, Tag: reflect.StructTag(f.Tag), Anonymous: f.Anonymous}
				}
				t = reflect.StructOf(fields)
			default:
				return nil, fmt.Errorf("invalid generated type kind %v", d.Kind)
			}
		}
		if t.Size() > uintptr(len(im.mem)) {
			return nil, fmt.Errorf("transfer type %s exceeds the image size", t)
		}
		im.types[id], im.typeIDs[t] = t, id
		return t, nil
	}
	for i := range im.typeDefs {
		if _, err := resolve(dynamicTypeBit | uintptr(i+1)); err != nil {
			return err
		}
	}
	return nil
}

func (w *imageWriter) copyReflectValue(v reflect.Value, dst uintptr, path string, depth int) error {
	if !v.IsValid() {
		return nil
	}
	if !v.CanInterface() {
		return fmt.Errorf("%s: reflected value has restricted access", path)
	}
	if v.CanAddr() {
		w.put(dst+16, 1)
		v = v.Addr()
	}
	id, err := w.typeID(v.Type())
	if err != nil {
		return err
	}
	slot, err := w.alloc(v.Type().Size(), v.Type().Align())
	if err != nil {
		return err
	}
	w.put(dst, id)
	w.put(dst+8, slot)
	return w.copy(v, slot, path+".Value", depth+1)
}

func (r *imageReader) restoreReflectValue(dst reflect.Value, src uintptr, path string, depth int) error {
	id, _ := r.word(src)
	slot, _ := r.word(src + 8)
	flags, _ := r.word(src + 16)
	if id == 0 {
		if slot != 0 || flags != 0 {
			return fmt.Errorf("%s: invalid zero reflect.Value", path)
		}
		dst.SetZero()
		return nil
	}
	t := r.types[id]
	if t == nil || flags > 1 || flags == 1 && t.Kind() != reflect.Pointer {
		return fmt.Errorf("%s: invalid reflect.Value type or flags", path)
	}
	v := reflect.New(t).Elem()
	if err := r.copy(v, slot, path+".Value", depth+1); err != nil {
		return err
	}
	if flags == 1 {
		if v.IsNil() {
			return fmt.Errorf("%s: nil reflected storage", path)
		}
		v = v.Elem()
	} else {
		// Boxing an interface directly loses its declared interface type (and a
		// nil interface becomes an invalid Value). A struct field preserves it.
		box := reflect.New(reflect.StructOf([]reflect.StructField{{Name: "Value", Type: t}})).Elem()
		box.Field(0).Set(v)
		v = reflect.ValueOf(box.Interface()).Field(0)
	}
	dst.Set(reflect.ValueOf(v))
	return nil
}
