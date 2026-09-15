//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"debug/dwarf"
	"debug/elf"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"unsafe"
)

const (
	goRuntimeType   dwarf.Attr = 0x2904
	goClosureOffset dwarf.Attr = 0x2907
	nativeImageTag             = 0x4c4c4e4154495631
)

//go:linkname reflectType reflect.toType
func reflectType(unsafe.Pointer) reflect.Type

func typeAddress(t reflect.Type) uintptr {
	return uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&t))[1])
}

type nativeLayout struct {
	name      string
	signature reflect.Type
	storage   reflect.Type
}

type capture struct {
	name string
	off  uintptr
	typ  dwarf.Offset
}

type nativeMetadata struct {
	types     map[uintptr]reflect.Type
	entries   map[dwarf.Offset]*dwarf.Entry
	names     map[uintptr]string
	captures  map[uintptr][]capture
	readOnly  [][2]uintptr
	typeStart uintptr
	mainPC    uintptr
	mainSize  uint64
	ixgoPC    uintptr
}

func loadMetadata() (*nativeMetadata, error) {
	// These private runtime entry points are checked against this toolchain.
	if runtime.Version() != "go1.26.6" {
		return nil, fmt.Errorf("sandbox closure ABI requires go1.26.6, got %s", runtime.Version())
	}
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if f.Type != elf.ET_EXEC {
		return nil, fmt.Errorf("sandbox requires a non-PIE ET_EXEC executable, got %s", f.Type)
	}
	syms, err := f.Symbols()
	if err != nil {
		return nil, fmt.Errorf("sandbox needs ELF symbols: %w", err)
	}
	m := &nativeMetadata{types: make(map[uintptr]reflect.Type), entries: make(map[dwarf.Offset]*dwarf.Entry), names: make(map[uintptr]string), captures: make(map[uintptr][]capture)}
	var end uintptr
	for _, s := range syms {
		switch s.Name {
		case "runtime.types":
			m.typeStart = uintptr(s.Value)
		case "runtime.etypes":
			end = uintptr(s.Value)
		case "main.main":
			if elf.ST_TYPE(s.Info) == elf.STT_FUNC {
				m.mainPC = uintptr(s.Value)
				m.mainSize = s.Size
			}
		case "github.com/goplus/ixgo.(*function).makeFunction.func1":
			m.ixgoPC = uintptr(s.Value)
		}
	}
	if m.typeStart == 0 || end <= m.typeStart {
		return nil, fmt.Errorf("missing runtime.types bounds")
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_W == 0 {
			m.readOnly = append(m.readOnly, [2]uintptr{uintptr(p.Vaddr), uintptr(p.Vaddr + p.Memsz)})
		}
	}
	d, err := f.DWARF()
	if err != nil {
		return nil, fmt.Errorf("sandbox needs capture DWARF: %w", err)
	}
	r := d.Reader()
	var owners []uintptr
	for {
		e, err := r.Next()
		if err != nil {
			return nil, err
		}
		if e == nil {
			break
		}
		if e.Tag == 0 {
			if len(owners) == 0 {
				return nil, fmt.Errorf("invalid DWARF nesting")
			}
			owners = owners[:len(owners)-1]
			continue
		}
		m.entries[e.Offset] = e
		if off, ok := e.Val(goRuntimeType).(uint64); ok && off != 0 {
			addr := m.typeStart + uintptr(off)
			if addr < m.typeStart || addr >= end || addr%8 != 0 {
				return nil, fmt.Errorf("invalid runtime type offset %#x", off)
			}
			m.types[addr] = reflectType(unsafe.Pointer(addr))
		}
		var owner uintptr
		if len(owners) != 0 {
			owner = owners[len(owners)-1]
		}
		if e.Tag == dwarf.TagSubprogram {
			pc, _ := e.Val(dwarf.AttrLowpc).(uint64)
			owner = uintptr(pc)
			m.names[owner], _ = e.Val(dwarf.AttrName).(string)
		} else if e.Tag == dwarf.TagInlinedSubroutine {
			owner = 0
		}
		if off, ok := e.Val(goClosureOffset).(int64); ok && off > 0 && owner != 0 {
			typ, ok := e.Val(dwarf.AttrType).(dwarf.Offset)
			if !ok {
				return nil, fmt.Errorf("closure capture lacks a DWARF type")
			}
			name, _ := e.Val(dwarf.AttrName).(string)
			m.captures[owner] = append(m.captures[owner], capture{name, uintptr(off), typ})
		}
		if e.Children {
			owners = append(owners, owner)
		}
	}
	return m, nil
}

func (m *nativeMetadata) layout(pc uintptr, signature reflect.Type) (nativeLayout, error) {
	name := m.names[pc]
	l := nativeLayout{name: name, signature: signature}
	if name == "" || strings.Contains(name, "[") || strings.HasSuffix(name, "-fm") {
		return l, fmt.Errorf("function %#x (%s) has no supported closure layout", pc, name)
	}
	vars := append([]capture(nil), m.captures[pc]...)
	sort.Slice(vars, func(i, j int) bool { return vars[i].off < vars[j].off })
	fields := []reflect.StructField{{Name: "F", Type: reflect.TypeFor[uintptr]()}}
	end := uintptr(8)
	for _, v := range vars {
		e := m.entries[v.typ]
		var typ reflect.Type
		for e != nil {
			if off, ok := e.Val(goRuntimeType).(uint64); ok && off != 0 {
				typ = m.types[m.typeStart+uintptr(off)]
				break
			}
			if e.Tag != dwarf.TagTypedef {
				break
			}
			off, ok := e.Val(dwarf.AttrType).(dwarf.Offset)
			if !ok {
				break
			}
			e = m.entries[off]
		}
		if typ == nil || v.off < end || v.off%uintptr(typ.Align()) != 0 {
			return l, fmt.Errorf("unsupported capture %s.%s at +%d", name, v.name, v.off)
		}
		if v.off > end {
			fields = append(fields, reflect.StructField{Name: fmt.Sprintf("Pad%d", len(fields)), Type: reflect.ArrayOf(int(v.off-end), reflect.TypeFor[byte]()), Offset: end})
		}
		fields = append(fields, reflect.StructField{Name: fmt.Sprintf("X%d", len(fields)), Type: typ, Offset: v.off})
		end = v.off + typ.Size()
	}
	l.storage = reflect.StructOf(fields)
	for i, f := range fields {
		if l.storage.Field(i).Offset != f.Offset {
			return l, fmt.Errorf("incompatible capture offset for %s", name)
		}
	}
	return l, nil
}

// Layouts below match Go 1.26.6 runtime/mbitmap.go. Only allocation bounds and
// GC pointer slots are read; runtime ownership and allocator state stay local.
type typePointers struct {
	elem, addr, mask uintptr
	typ              unsafe.Pointer
}

//go:linkname findObject runtime.findObject
func findObject(p, refBase, refOff uintptr) (base uintptr, span unsafe.Pointer, index uintptr)

//go:linkname spanLayout runtime.(*mspan).layout
func spanLayout(span unsafe.Pointer) (size, n, total uintptr)

//go:linkname objectPointers runtime.(*mspan).typePointersOfUnchecked
func objectPointers(span unsafe.Pointer, addr uintptr) typePointers

//go:linkname nextPointer runtime.typePointers.next
func nextPointer(tp typePointers, limit uintptr) (typePointers, uintptr)

func (w *imageWriter) exportNative(src reflect.Value, layout nativeLayout, path string, depth int) (uintptr, error) {
	root := functionAddress(src)
	ref := objectRef{typ: src.Type(), addr: root, span: -2}
	if old, ok := w.refs[ref]; ok {
		return old, nil
	}
	base, span, _ := findObject(root, 0, 0)
	if base == 0 {
		ok := false
		for _, region := range w.metadata.readOnly {
			if root >= region[0] && root+8 <= region[1] && layout.storage.Size() == 8 {
				ok = true
				break
			}
		}
		if !ok {
			return 0, fmt.Errorf("%s: closure is neither a heap object nor a static function", path)
		}
	} else {
		size, _, _ := spanLayout(span)
		if root-base > size || layout.storage.Size() > size-(root-base) {
			return 0, fmt.Errorf("%s: missing or oversized closure capture layout", path)
		}
		shape := reflect.New(layout.storage)
		shapeBase, shapeSpan, _ := findObject(shape.Pointer(), 0, 0)
		actual, expected := objectPointers(span, base), objectPointers(shapeSpan, shapeBase)
		for {
			var a, b uintptr
			actual, a = nextPointer(actual, root+layout.storage.Size())
			expected, b = nextPointer(expected, shape.Pointer()+layout.storage.Size())
			if a == 0 && b == 0 {
				break
			}
			if a == 0 || b == 0 || a-root != b-shape.Pointer() {
				return 0, fmt.Errorf("%s: capture DWARF disagrees with GC pointer slots", path)
			}
		}
		runtime.KeepAlive(shape)
	}
	header, err := w.alloc(24, 8)
	if err != nil {
		return 0, err
	}
	payload, err := w.alloc(layout.storage.Size(), layout.storage.Align())
	if err != nil {
		return 0, err
	}
	w.refs[ref] = header
	w.retain(ref, src)
	w.put(header, nativeImageTag)
	w.put(header+8, src.Pointer())
	w.put(header+16, payload)
	w.put(payload, src.Pointer())
	for i := 1; i < layout.storage.NumField(); i++ {
		field := layout.storage.Field(i)
		if field.Name[0] == 'P' {
			continue
		}
		v := reflect.NewAt(field.Type, unsafe.Pointer(root+field.Offset)).Elem()
		if err := w.copy(v, payload+field.Offset, path+"."+field.Name, depth+1); err != nil {
			return 0, err
		}
	}
	return header, nil
}

func (r *imageReader) restoreNative(header uintptr, typ reflect.Type, path string, depth int) (reflect.Value, error) {
	ref := objectRef{typ: typ, addr: header, span: -2}
	if v, ok := r.refs[ref]; ok {
		return v, nil
	}
	if _, err := r.bytes(header, 24); err != nil {
		return reflect.Value{}, err
	}
	tag, _ := r.word(header)
	entry, _ := r.word(header + 8)
	payload, _ := r.word(header + 16)
	layout, ok := r.native[entry]
	if tag != nativeImageTag || !ok || layout.signature != typ {
		return reflect.Value{}, fmt.Errorf("%s: unauthorized function entry/signature %#x", path, entry)
	}
	if _, err := r.bytes(payload, layout.storage.Size()); err != nil {
		return reflect.Value{}, err
	}
	code, _ := r.word(payload)
	if code != entry {
		return reflect.Value{}, fmt.Errorf("%s: closure code does not match entry", path)
	}
	fn, bound := r.bindings[ref]
	var storage reflect.Value
	if bound {
		storage = reflect.NewAt(layout.storage, unsafe.Pointer(functionAddress(fn)))
	} else {
		storage = reflect.New(layout.storage)
		fn = reflect.New(typ).Elem()
		*(*unsafe.Pointer)(unsafe.Pointer(fn.UnsafeAddr())) = storage.UnsafePointer()
		storage.Elem().Field(0).SetUint(uint64(entry))
	}
	r.refs[ref] = fn
	for i := 1; i < layout.storage.NumField(); i++ {
		field := layout.storage.Field(i)
		if field.Name[0] == 'P' {
			continue
		}
		if err := r.copy(storage.Elem().Field(i), payload+field.Offset, path+"."+field.Name, depth+1); err != nil {
			return reflect.Value{}, err
		}
	}
	runtime.KeepAlive(storage)
	return fn, nil
}
