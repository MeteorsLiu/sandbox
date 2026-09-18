package state

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

type nativeMetadata struct {
	path               string
	machine            elf.Machine
	typeStart, typeEnd uintptr
	funcStart, funcEnd uintptr
	newobject          uint64
	functions          map[uintptr]elf.Symbol
	names              map[string]elf.Symbol
	methods            map[string]reflect.Type
	segments           []elf.ProgHeader

	mu      sync.Mutex // Protects layouts and the set of already scanned factories.
	layouts map[uintptr]reflect.Type
	scanned map[string]bool
}

// Only immutable executable metadata and layouts are shared. Each Save/Load
// still owns its object graph and restored closure allocations.
var executableNativeMetadata = sync.OnceValues(loadNativeMetadata)

func loadNativeMetadata() (*nativeMetadata, error) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return nil, fmt.Errorf("native closures require Linux amd64 or arm64")
	}
	if runtime.Version() != "go1.26.6" {
		return nil, fmt.Errorf("native closure ABI requires go1.26.6, got %s", runtime.Version())
	}
	// Reopening this path still refers to the running image after an upgrade
	// replaces or unlinks its original pathname.
	const path = "/proc/self/exe"
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if f.Type != elf.ET_EXEC || f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB {
		return nil, fmt.Errorf("native closures require a little-endian 64-bit non-PIE ET_EXEC executable")
	}
	syms, err := f.Symbols()
	if err != nil {
		return nil, fmt.Errorf("native closures need ELF symbols: %w", err)
	}
	m := &nativeMetadata{
		path: path, machine: f.Machine,
		functions: make(map[uintptr]elf.Symbol), names: make(map[string]elf.Symbol),
		methods: make(map[string]reflect.Type),
		layouts: make(map[uintptr]reflect.Type), scanned: make(map[string]bool),
	}
	var funcStart, funcEnd elf.Symbol
	for _, s := range syms {
		switch s.Name {
		case "runtime.types":
			m.typeStart = uintptr(s.Value)
		case "runtime.etypes":
			m.typeEnd = uintptr(s.Value)
		case "runtime.newobject":
			m.newobject = s.Value
		case "go:funcdesc":
			funcStart = s
		case "runtime.gcbits.*":
			funcEnd = s
		}
		if elf.ST_TYPE(s.Info) == elf.STT_FUNC && s.Size != 0 {
			m.functions[uintptr(s.Value)] = s
			m.names[s.Name] = s
		}
	}
	if m.typeStart == 0 || m.typeEnd <= m.typeStart || m.newobject == 0 {
		return nil, fmt.Errorf("missing native closure ELF symbols")
	}
	// Go 1.26.6 WriteFuncSyms emits one PC per capture-free function value.
	// The linker groups these under go:funcdesc, followed by runtime.gcbits.*.
	// Keep only the range: a funcval inside it has no captured environment.
	if funcStart.Section == elf.SHN_UNDEF || funcStart.Section != funcEnd.Section || int(funcStart.Section) >= len(f.Sections) || funcStart.Value > funcEnd.Value || (funcEnd.Value-funcStart.Value)%8 != 0 {
		return nil, fmt.Errorf("invalid native function descriptor range")
	}
	funcSection := f.Sections[funcStart.Section]
	if funcStart.Value < funcSection.Addr || funcEnd.Value-funcSection.Addr > funcSection.Size {
		return nil, fmt.Errorf("native function descriptors extend beyond their ELF section")
	}
	m.funcStart, m.funcEnd = uintptr(funcStart.Value), uintptr(funcEnd.Value)
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 {
			m.segments = append(m.segments, p.ProgHeader)
		}
	}
	section := f.Section(".typelink")
	if section == nil {
		return nil, fmt.Errorf("missing native receiver type links")
	}
	data, err := section.Data()
	if err != nil {
		return nil, err
	}
	// Typelinks usually keeps *T rather than T. Index both method sets using
	// the compiler's symbol construction, including qualified private names.
	seen := make(map[reflect.Type]bool)
	for len(data) >= 4 {
		offset := int32(binary.LittleEndian.Uint32(data))
		typ := nativeReflectType(unsafe.Pointer(m.typeStart + uintptr(offset)))
		data = data[4:]
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Name() == "" || seen[typ] {
			continue
		}
		seen[typ] = true
		m.indexMethods(typ)
		m.indexMethods(reflect.PointerTo(typ))
	}
	return m, nil
}

// captureFree comes from a static funcval address when saving, or an empty Env
// reference when loading. Cache it so restored heap funcvals can be saved again.
func (m *nativeMetadata) layout(pc uintptr, captureFree bool) (reflect.Type, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sym := m.functions[pc]
	name := strings.TrimSuffix(sym.Name, ".abi0")
	if pc == 0 || name == "" || strings.Contains(name, "[") || strings.Contains(name, "-range") || name == "reflect.makeFuncStub" || name == "reflect.methodValueCall" || captureFree && strings.HasSuffix(name, "-fm") {
		return nil, fmt.Errorf("function %#x (%s) has no supported native closure layout", pc, name)
	}
	if typ := m.layouts[pc]; typ != nil {
		if captureFree && typ.NumField() != 1 {
			return nil, fmt.Errorf("function %#x (%s) has a captured environment", pc, name)
		}
		return typ, nil
	}
	// The linker redirects dead methods to this capture-free throw stub.
	// reflectx.MethodX can still expose it through a heap-allocated funcval.
	if captureFree || name == "runtime.unreachableMethod" {
		typ := reflect.TypeFor[struct{ F uintptr }]()
		m.layouts[pc] = typ
		return typ, nil
	}
	boundMethod := strings.HasSuffix(name, "-fm")
	receiver := m.methods[strings.TrimSuffix(name, "-fm")]
	if boundMethod && receiver == nil {
		return nil, fmt.Errorf("method %s has no unique static receiver type", name)
	}
	if receiver != nil {
		// reflect.Type.Method creates a heap funcval for T.M or (*T).M.
		// Its receiver is an argument; only M-fm captures a receiver.
		layout := reflect.TypeFor[struct{ F uintptr }]()
		if boundMethod {
			// MethodValueType uses F + R, including zero-sized receivers.
			layout = reflect.StructOf([]reflect.StructField{
				{Name: "F", Type: reflect.TypeFor[uintptr]()},
				{Name: "R", Type: receiver},
			})
		}
		m.layouts[pc] = layout
		return layout, nil
	}
	// Try lexical parents, including inlining prefixes. For p.F.factory.func1,
	// p.F.factory may not exist, while p.F contains the inlined allocation.
	// Never fall back to scanning all of .text.
	for parent := name; ; {
		i := strings.LastIndexByte(parent, '.')
		if i < 0 {
			break
		}
		parent = parent[:i]
		factory, ok := m.names[parent]
		if !ok || m.scanned[parent] {
			continue
		}
		code, err := m.functionCode(factory)
		if err != nil {
			return nil, err
		}
		for entry, addr := range closureAllocations(m.machine, code, factory.Value, m.newobject) {
			entryName := m.functions[entry].Name
			// A factory can also allocate bound methods (F + R). They do not
			// describe its nested closures, whose symbols start with parent.
			if !strings.HasPrefix(entryName, parent+".") || strings.HasSuffix(entryName, "-fm") {
				continue
			}
			typ, err := m.environmentType(addr)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", parent, err)
			}
			if prev := m.layouts[entry]; prev != nil && prev != typ {
				return nil, fmt.Errorf("ambiguous closure layout for %#x", entry)
			}
			m.layouts[entry] = typ
		}
		m.scanned[parent] = true
		if typ := m.layouts[pc]; typ != nil {
			return typ, nil
		}
	}
	return nil, fmt.Errorf("function %#x (%s) has no supported native closure layout: no matching allocation in its enclosing functions", pc, name)
}

func (m *nativeMetadata) functionCode(sym elf.Symbol) ([]byte, error) {
	for _, p := range m.segments {
		if sym.Value < p.Vaddr {
			continue
		}
		off := sym.Value - p.Vaddr
		if off > p.Filesz || sym.Size > p.Filesz-off {
			continue
		}
		f, err := os.Open(m.path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		code := make([]byte, sym.Size)
		_, err = f.ReadAt(code, int64(p.Off+off))
		return code, err
	}
	return nil, fmt.Errorf("function %s has no executable ELF range", sym.Name)
}

func (m *nativeMetadata) environmentType(addr uintptr) (reflect.Type, error) {
	// The newobject argument points into this same non-PIE executable's mapped
	// type section. Noalg descriptors exist there even though typelinks omits them.
	if addr < m.typeStart || addr > m.typeEnd-80 || addr%8 != 0 {
		return nil, fmt.Errorf("invalid closure type address %#x", addr)
	}
	actual := nativeReflectType(unsafe.Pointer(addr))
	if actual.Kind() != reflect.Struct || actual.NumField() < 2 {
		return nil, fmt.Errorf("invalid closure environment type %v", actual)
	}
	fields := make([]reflect.StructField, actual.NumField())
	for i := range fields {
		f := actual.Field(i)
		name := fmt.Sprintf("X%d", i-1)
		if i == 0 {
			name = "F"
			if f.Type != reflect.TypeFor[uintptr]() || f.Offset != 0 {
				return nil, fmt.Errorf("invalid closure PC field %v", f)
			}
		}
		if f.Name != name {
			return nil, fmt.Errorf("invalid closure capture field %q", f.Name)
		}
		fields[i] = f
	}
	// Use a normal struct descriptor so reflecttype can export the environment
	// without depending on reflect's omission of noalg types from typelinks.
	typ := reflect.StructOf(fields)
	if typ.Size() != actual.Size() {
		return nil, fmt.Errorf("closure environment size mismatch")
	}
	for i, f := range fields {
		if typ.Field(i).Offset != f.Offset {
			return nil, fmt.Errorf("closure capture offset mismatch at +%d", f.Offset)
		}
	}
	return typ, nil
}
