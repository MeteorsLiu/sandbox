package state

import (
	"debug/dwarf"
	"debug/elf"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unsafe"
)

const (
	goRuntimeType   dwarf.Attr = 0x2904
	goClosureOffset dwarf.Attr = 0x2907
)

//go:linkname nativeReflectType reflect.toType
func nativeReflectType(unsafe.Pointer) reflect.Type

func nativeTypeAddress(typ reflect.Type) uintptr {
	return uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&typ))[1])
}

type nativeCapture struct {
	offset uintptr
	typ    dwarf.Offset
}

type nativeMetadata struct {
	typeStart uintptr
	types     map[uintptr]reflect.Type
	entries   map[dwarf.Offset]*dwarf.Entry
	names     map[uintptr]string
	captures  map[uintptr][]nativeCapture
}

// The executable metadata is immutable. Graphs and synthesized closure layouts
// belong to each Save/Load, so concurrent transfers share no mutable graph state.
var executableNativeMetadata = sync.OnceValues(loadNativeMetadata)

type nativeState struct {
	layouts map[uintptr]reflect.Type
	storage map[reflect.Type]uintptr
}

func (*nativeState) metadata() *nativeMetadata {
	m, err := executableNativeMetadata()
	if err != nil {
		Failf("native closure metadata: %w", err)
	}
	return m
}

func loadNativeMetadata() (*nativeMetadata, error) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return nil, fmt.Errorf("native closures require Linux amd64 or arm64")
	}
	if runtime.Version() != "go1.26.6" {
		return nil, fmt.Errorf("native closure ABI requires go1.26.6, got %s", runtime.Version())
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
		return nil, fmt.Errorf("native closures require a non-PIE ET_EXEC executable, got %s", f.Type)
	}
	syms, err := f.Symbols()
	if err != nil {
		return nil, fmt.Errorf("native closures need ELF symbols: %w", err)
	}
	m := &nativeMetadata{
		types:    make(map[uintptr]reflect.Type),
		entries:  make(map[dwarf.Offset]*dwarf.Entry),
		names:    make(map[uintptr]string),
		captures: make(map[uintptr][]nativeCapture),
	}
	var typeEnd uintptr
	for _, s := range syms {
		switch s.Name {
		case "runtime.types":
			m.typeStart = uintptr(s.Value)
		case "runtime.etypes":
			typeEnd = uintptr(s.Value)
		}
	}
	if m.typeStart == 0 || typeEnd <= m.typeStart {
		return nil, fmt.Errorf("missing runtime.types bounds")
	}
	d, err := f.DWARF()
	if err != nil {
		return nil, fmt.Errorf("native closures need capture DWARF: %w", err)
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
			if addr < m.typeStart || addr >= typeEnd || addr%8 != 0 {
				return nil, fmt.Errorf("invalid runtime type offset %#x", off)
			}
			m.types[addr] = nativeReflectType(unsafe.Pointer(addr))
		}
		var owner uintptr
		if len(owners) != 0 {
			owner = owners[len(owners)-1]
		}
		switch e.Tag {
		case dwarf.TagSubprogram:
			pc, _ := e.Val(dwarf.AttrLowpc).(uint64)
			owner = uintptr(pc)
			m.names[owner], _ = e.Val(dwarf.AttrName).(string)
		case dwarf.TagInlinedSubroutine:
			owner = 0
		}
		if off, ok := e.Val(goClosureOffset).(int64); ok && off > 0 && owner != 0 {
			typ, ok := e.Val(dwarf.AttrType).(dwarf.Offset)
			if !ok {
				return nil, fmt.Errorf("closure capture lacks a DWARF type")
			}
			m.captures[owner] = append(m.captures[owner], nativeCapture{uintptr(off), typ})
		}
		if e.Children {
			owners = append(owners, owner)
		}
	}
	return m, nil
}

func (ns *nativeState) layout(pc uintptr) reflect.Type {
	if typ := ns.layouts[pc]; typ != nil {
		return typ
	}
	m := ns.metadata()
	name := m.names[pc]
	if pc == 0 || name == "" || strings.Contains(name, "[") || strings.HasSuffix(name, "-fm") || name == "reflect.makeFuncStub" {
		Failf("function %#x (%s) has no supported native closure layout", pc, name)
	}
	captures := append([]nativeCapture(nil), m.captures[pc]...)
	sort.Slice(captures, func(i, j int) bool { return captures[i].offset < captures[j].offset })
	fields := []reflect.StructField{{Name: "F", Type: reflect.TypeFor[uintptr]()}}
	end := uintptr(8)
	for i, c := range captures {
		e := m.entries[c.typ]
		var typ reflect.Type
		for e != nil {
			if off, ok := e.Val(goRuntimeType).(uint64); ok && off != 0 {
				typ = m.types[m.typeStart+uintptr(off)]
				break
			}
			if e.Tag != dwarf.TagTypedef {
				break
			}
			off, _ := e.Val(dwarf.AttrType).(dwarf.Offset)
			e = m.entries[off]
		}
		if typ == nil || c.offset < end || c.offset%uintptr(typ.Align()) != 0 {
			Failf("unsupported capture %s at +%d", name, c.offset)
		}
		if c.offset > end {
			fields = append(fields, reflect.StructField{
				Name: fmt.Sprintf("Pad%d", i), Type: reflect.ArrayOf(int(c.offset-end), reflect.TypeFor[byte]()), Offset: end,
			})
		}
		fields = append(fields, reflect.StructField{Name: fmt.Sprintf("X%d", i), Type: typ, Offset: c.offset})
		end = c.offset + typ.Size()
	}
	typ := reflect.StructOf(fields)
	for i, f := range fields {
		if typ.Field(i).Offset != f.Offset {
			Failf("incompatible capture offset for %s at +%d", name, f.Offset)
		}
	}
	if ns.layouts == nil {
		ns.layouts = make(map[uintptr]reflect.Type)
		ns.storage = make(map[reflect.Type]uintptr)
	}
	ns.layouts[pc] = typ
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
