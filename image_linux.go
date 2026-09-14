//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"runtime"
	"unsafe"
)

// Images contain native scalar/struct layouts and relocated pointers. Interface
// slots contain a concrete type address and a boxed value address. Map slots
// point to {count, keys, values}; the receiver rebuilds its own itabs and maps.
// Images are never passed directly to application code as live Go objects.
type valueImage struct {
	mem      []byte
	used     uintptr
	types    map[uintptr]reflect.Type
	metadata *nativeMetadata
	native   map[uintptr]nativeLayout
}

type objectRef struct {
	typ  reflect.Type
	addr uintptr
	span int
}

type imageWriter struct {
	*valueImage
	refs     map[objectRef]uintptr
	keep     []reflect.Value
	anchors  []reflect.Value
	retained map[objectRef]bool
}

type imageReader struct {
	*valueImage
	refs     map[objectRef]reflect.Value
	bindings map[objectRef]reflect.Value
}

func addressable(v reflect.Value) reflect.Value {
	if v.CanAddr() {
		return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
	}
	copy := reflect.New(v.Type()).Elem()
	copy.Set(v)
	return copy
}

func functionAddress(v reflect.Value) uintptr {
	v = addressable(v)
	addr := *(*uintptr)(unsafe.Pointer(v.UnsafeAddr()))
	runtime.KeepAlive(v)
	return addr
}

func (im *valueImage) bytes(addr, size uintptr) ([]byte, error) {
	if addr < 4096 || addr > im.used || size > im.used-addr {
		return nil, fmt.Errorf("reference %#x size=%d is outside image [%#x,%#x)", addr, size, 4096, im.used)
	}
	return im.mem[addr : addr+size], nil
}

func (im *valueImage) word(addr uintptr) (uintptr, error) {
	b, err := im.bytes(addr, 8)
	if err != nil {
		return 0, err
	}
	return uintptr(binary.LittleEndian.Uint64(b)), nil
}

func (w *imageWriter) put(addr, value uintptr) {
	binary.LittleEndian.PutUint64(w.mem[addr:], uint64(value))
}

func (w *imageWriter) alloc(size uintptr, align int) (uintptr, error) {
	if size == 0 {
		size = 1
	}
	off := (w.used + uintptr(align) - 1) &^ (uintptr(align) - 1)
	if off > uintptr(len(w.mem)) || size > uintptr(len(w.mem))-off {
		return 0, fmt.Errorf("argument image exceeds %d bytes", len(w.mem))
	}
	clear(w.mem[off : off+size])
	w.used = off + size
	return off, nil
}

func (w *imageWriter) copy(src reflect.Value, dst uintptr, path string, depth int) error {
	if depth > 256 {
		return fmt.Errorf("%s: graph nesting exceeds 256", path)
	}
	src = addressable(src)
	if err := transferable(src.Type()); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	w.keep = append(w.keep, src)
	t := src.Type()
	if _, ok := w.types[typeAddress(t)]; !ok {
		return fmt.Errorf("%s: type %s was not included in the static transfer type set", path, t)
	}
	ref := objectRef{typ: t, addr: src.UnsafeAddr()}
	if old, ok := w.refs[ref]; ok && old != dst && t.Size() != 0 {
		return fmt.Errorf("%s: overlapping/interior storage requires a common allocation image", path)
	}
	w.refs[ref] = dst
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if err := w.copy(src.Field(i), dst+f.Offset, path+"."+f.Name, depth+1); err != nil {
				return err
			}
		}
	case reflect.Array:
		for i := 0; i < src.Len(); i++ {
			if err := w.copy(src.Index(i), dst+uintptr(i)*t.Elem().Size(), path+"[]", depth+1); err != nil {
				return err
			}
		}
	case reflect.Pointer:
		if src.IsNil() {
			return nil
		}
		ref := objectRef{typ: t.Elem(), addr: src.Pointer()}
		addr, found := w.refs[ref]
		if !found {
			var err error
			addr, err = w.alloc(t.Elem().Size(), t.Elem().Align())
			if err != nil {
				return err
			}
			w.refs[ref] = addr
			if err := w.copy(src.Elem(), addr, path+"*", depth+1); err != nil {
				return err
			}
		}
		w.retain(ref, src)
		w.put(dst, addr)
	case reflect.String:
		s := src.String()
		if len(s) == 0 {
			return nil
		}
		addr, err := w.alloc(uintptr(len(s)), 1)
		if err != nil {
			return err
		}
		copy(w.mem[addr:], s)
		w.put(dst, addr)
		w.put(dst+8, uintptr(len(s)))
	case reflect.Slice:
		if src.IsNil() {
			return nil
		}
		ref := objectRef{typ: t, addr: src.Pointer(), span: src.Cap() + 1}
		addr, found := w.refs[ref]
		if !found {
			var err error
			addr, err = w.alloc(uintptr(src.Cap())*t.Elem().Size(), t.Elem().Align())
			if err != nil {
				return err
			}
			w.refs[ref] = addr
			full := src.Slice(0, src.Cap())
			for i := 0; i < full.Len(); i++ {
				if err := w.copy(full.Index(i), addr+uintptr(i)*t.Elem().Size(), path+"[]", depth+1); err != nil {
					return err
				}
			}
		}
		w.retain(ref, src)
		w.put(dst, addr)
		w.put(dst+8, uintptr(src.Len()))
		w.put(dst+16, uintptr(src.Cap()))
	case reflect.Interface:
		if src.IsNil() {
			return nil
		}
		v := src.Elem()
		addr, err := w.alloc(v.Type().Size(), v.Type().Align())
		if err != nil {
			return err
		}
		if err := w.copy(v, addr, path+".("+v.Type().String()+")", depth+1); err != nil {
			return err
		}
		w.put(dst, typeAddress(v.Type()))
		w.put(dst+8, addr)
	case reflect.Map:
		if src.IsNil() {
			return nil
		}
		ref := objectRef{typ: t, addr: uintptr(src.UnsafePointer()), span: -1}
		if addr, found := w.refs[ref]; found {
			w.put(dst, addr)
			return nil
		}
		header, err := w.alloc(24, 8)
		if err != nil {
			return err
		}
		w.refs[ref] = header
		w.retain(ref, src)
		keys, err := w.alloc(uintptr(src.Len())*t.Key().Size(), t.Key().Align())
		if err != nil {
			return err
		}
		values, err := w.alloc(uintptr(src.Len())*t.Elem().Size(), t.Elem().Align())
		if err != nil {
			return err
		}
		w.put(dst, header)
		w.put(header, uintptr(src.Len()))
		w.put(header+8, keys)
		w.put(header+16, values)
		iter := src.MapRange()
		for i := uintptr(0); iter.Next(); i++ {
			if err := w.copy(iter.Key(), keys+i*t.Key().Size(), path+"{key}", depth+1); err != nil {
				return err
			}
			if err := w.copy(iter.Value(), values+i*t.Elem().Size(), path+"{value}", depth+1); err != nil {
				return err
			}
		}
	case reflect.Func:
		if src.IsNil() {
			return nil
		}
		layout, ok := w.native[src.Pointer()]
		if !ok {
			var err error
			layout, err = w.metadata.layout(src.Pointer(), t)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			w.native[src.Pointer()] = layout
		}
		if layout.signature != t {
			return fmt.Errorf("%s: conflicting function signatures", path)
		}
		image, err := w.exportNative(src, layout, path, depth)
		if err != nil {
			return err
		}
		w.put(dst, image)
	case reflect.Chan, reflect.UnsafePointer:
		if !src.IsNil() {
			return fmt.Errorf("%s: non-nil %s has no transferable object layout", path, t.Kind())
		}
	default:
		copy(w.mem[dst:dst+t.Size()], unsafe.Slice((*byte)(unsafe.Pointer(src.UnsafeAddr())), int(t.Size())))
	}
	return nil
}

func (r *imageReader) span(addr, count uintptr, t reflect.Type) error {
	if count > uintptr(len(r.mem)) || (t.Size() > 0 && count > uintptr(len(r.mem))/t.Size()) {
		return fmt.Errorf("invalid %s element count %d", t, count)
	}
	_, err := r.bytes(addr, count*t.Size())
	return err
}

func (r *imageReader) copy(dst reflect.Value, src uintptr, path string, depth int) error {
	if depth > 256 {
		return fmt.Errorf("%s: graph nesting exceeds 256", path)
	}
	dst = addressable(dst)
	t := dst.Type()
	if err := transferable(t); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if _, err := r.bytes(src, t.Size()); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if src%uintptr(t.Align()) != 0 {
		return fmt.Errorf("%s: unaligned %s address %#x", path, t, src)
	}
	r.refs[objectRef{typ: t, addr: src}] = dst.Addr()
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if err := r.copy(dst.Field(i), src+f.Offset, path+"."+f.Name, depth+1); err != nil {
				return err
			}
		}
	case reflect.Array:
		for i := 0; i < dst.Len(); i++ {
			if err := r.copy(dst.Index(i), src+uintptr(i)*t.Elem().Size(), path+"[]", depth+1); err != nil {
				return err
			}
		}
	case reflect.Pointer:
		addr, _ := r.word(src)
		if addr == 0 {
			dst.SetZero()
			return nil
		}
		ref := objectRef{typ: t.Elem(), addr: addr}
		if v, found := r.refs[ref]; found {
			dst.Set(v)
			return nil
		}
		p, bound := r.bindings[ref]
		if !bound {
			p = reflect.New(t.Elem())
		}
		r.refs[ref] = p
		if err := r.copy(p.Elem(), addr, path+"*", depth+1); err != nil {
			return err
		}
		dst.Set(p)
	case reflect.String:
		addr, _ := r.word(src)
		n, _ := r.word(src + 8)
		if n == 0 {
			dst.SetString("")
			return nil
		}
		b, err := r.bytes(addr, n)
		if err != nil {
			return err
		}
		dst.SetString(string(b))
	case reflect.Slice:
		addr, _ := r.word(src)
		n, _ := r.word(src + 8)
		cap, _ := r.word(src + 16)
		if addr == 0 {
			if n != 0 || cap != 0 {
				return fmt.Errorf("%s: nil slice with nonzero length/capacity", path)
			}
			dst.SetZero()
			return nil
		}
		if n > cap {
			return fmt.Errorf("%s: slice length exceeds capacity", path)
		}
		if err := r.span(addr, cap, t.Elem()); err != nil {
			return err
		}
		ref := objectRef{typ: t, addr: addr, span: int(cap) + 1}
		if v, found := r.refs[ref]; found {
			dst.Set(v.Slice(0, int(n)))
			return nil
		}
		v, bound := r.bindings[ref]
		if !bound {
			v = reflect.MakeSlice(t, int(cap), int(cap))
		} else {
			v = v.Slice(0, int(cap))
		}
		r.refs[ref] = v
		for i := uintptr(0); i < cap; i++ {
			if err := r.copy(v.Index(int(i)), addr+i*t.Elem().Size(), path+"[]", depth+1); err != nil {
				return err
			}
		}
		dst.Set(v.Slice(0, int(n)))
	case reflect.Interface:
		typeAddr, _ := r.word(src)
		addr, _ := r.word(src + 8)
		if typeAddr == 0 {
			if addr != 0 {
				return fmt.Errorf("%s: nil interface with non-nil data", path)
			}
			dst.SetZero()
			return nil
		}
		concrete, ok := r.types[typeAddr]
		if !ok || !concrete.Implements(t) {
			return fmt.Errorf("%s: invalid concrete type %#x", path, typeAddr)
		}
		v := reflect.New(concrete).Elem()
		if err := r.copy(v, addr, path+".("+concrete.String()+")", depth+1); err != nil {
			return err
		}
		dst.Set(v)
	case reflect.Map:
		header, _ := r.word(src)
		if header == 0 {
			dst.SetZero()
			return nil
		}
		if _, err := r.bytes(header, 24); err != nil {
			return err
		}
		ref := objectRef{typ: t, addr: header, span: -1}
		if v, found := r.refs[ref]; found {
			dst.Set(v)
			return nil
		}
		n, _ := r.word(header)
		keys, _ := r.word(header + 8)
		values, _ := r.word(header + 16)
		if err := r.span(keys, n, t.Key()); err != nil {
			return err
		}
		if err := r.span(values, n, t.Elem()); err != nil {
			return err
		}
		v, bound := r.bindings[ref]
		if !bound {
			v = reflect.MakeMapWithSize(t, int(n))
		} else {
			v.Clear()
		}
		r.refs[ref] = v
		for i := uintptr(0); i < n; i++ {
			key := reflect.New(t.Key()).Elem()
			value := reflect.New(t.Elem()).Elem()
			if err := r.copy(key, keys+i*t.Key().Size(), path+"{key}", depth+1); err != nil {
				return err
			}
			if !key.Comparable() {
				return fmt.Errorf("%s: non-comparable map key", path)
			}
			if err := r.copy(value, values+i*t.Elem().Size(), path+"{value}", depth+1); err != nil {
				return err
			}
			v.SetMapIndex(key, value)
		}
		dst.Set(v)
	case reflect.Func:
		addr, _ := r.word(src)
		if addr == 0 {
			dst.SetZero()
			return nil
		}
		fn, err := r.restoreNative(addr, t, path, depth)
		if err != nil {
			return err
		}
		dst.Set(fn)
	case reflect.Chan, reflect.UnsafePointer:
		dst.SetZero()
		addr, _ := r.word(src)
		if addr != 0 {
			return fmt.Errorf("%s: unexpected non-nil %s", path, t.Kind())
		}
	default:
		b, _ := r.bytes(src, t.Size())
		copy(unsafe.Slice((*byte)(unsafe.Pointer(dst.UnsafeAddr())), int(t.Size())), b)
	}
	return nil
}
