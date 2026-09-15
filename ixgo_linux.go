//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"go/types"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"unsafe"

	"github.com/goplus/ixgo"
	"github.com/visualfc/funcval"
	"golang.org/x/tools/go/ssa"
)

const ixgoImageTag = 0x4c4c4958474f3031

// These prefixes are ixgo v1.1.6's function and makeFuncVal. Never interpret
// a MakeFunc callback with this layout before checking its actual entry point.
type ixgoFunction struct {
	Interp *ixgo.Interp
	Fn     *ssa.Function
}

type ixgoCallback struct {
	funcval.FuncVal
	interp *ixgo.Interp
	pfn    *ixgoFunction
	typ    reflect.Type
	env    []any
}

//go:linkname ixgoMakeFunction github.com/goplus/ixgo.(*function).makeFunction
func ixgoMakeFunction(pfn *ixgoFunction, typ reflect.Type, env []any) reflect.Value

//go:linkname ixgoToType github.com/goplus/ixgo.(*Interp).toType
func ixgoToType(interp *ixgo.Interp, typ types.Type) reflect.Type

//go:linkname ixgoBuildMain github.com/goplus/ixgo.(*Context).buildMain
func ixgoBuildMain(ctx *ixgo.Context, source *ixgo.SourcePackage) (*ssa.Package, error)

type ixgoSource struct {
	Path  string
	Names []string
	Files []string
}

type namedSlot struct {
	Name string
	Type uintptr
	Slot uintptr
}

type ixgoDescription struct {
	Main      string
	Mode      ixgo.Mode
	Builder   ssa.BuilderMode
	Sources   []ixgoSource
	Functions []string
	Globals   []namedSlot
	Overrides []namedSlot
}

type transferDescription struct {
	Programs []ixgoDescription
	Types    []transferType
}

type ixgoProgram struct {
	interp      *ixgo.Interp
	ctx         *ixgo.Context
	desc        ixgoDescription
	functions   []*ixgoFunction
	functionIDs map[*ssa.Function]int
	types       map[string]reflect.Type
	typeKeys    map[reflect.Type]string
	globals     map[string]any
	overrides   map[string]reflect.Value
}

func privateField(v any, name string) reflect.Value {
	return addressable(reflect.ValueOf(v).Elem().FieldByName(name))
}

func (m *nativeMetadata) ixgoCallback(v reflect.Value) (callback *ixgoCallback, err error) {
	if v.IsNil() {
		return nil, nil
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("reading function wrapper: %v", p)
		}
	}()
	fv, layers := funcval.Get(v.Interface())
	if layers == 0 {
		return nil, nil
	}
	if layers != 1 || m.ixgoPC == 0 || fv.Fn != m.ixgoPC {
		return nil, fmt.Errorf("unsupported reflect.MakeFunc callback")
	}
	c := (*ixgoCallback)(unsafe.Pointer(fv))
	if c.interp == nil || c.pfn == nil || c.pfn.Interp != c.interp || c.pfn.Fn == nil || c.typ != v.Type() || len(c.env) != len(c.pfn.Fn.FreeVars) {
		return nil, fmt.Errorf("ixgo callback metadata mismatch")
	}
	runtime.KeepAlive(v)
	return c, nil
}

func indexIxgo(interp *ixgo.Interp) (*ixgoProgram, error) {
	ctx := privateField(interp, "ctx").Interface().(*ixgo.Context)
	p := &ixgoProgram{interp: interp, ctx: ctx, functionIDs: make(map[*ssa.Function]int), types: make(map[string]reflect.Type), typeKeys: make(map[reflect.Type]string), overrides: make(map[string]reflect.Value)}
	p.globals = privateField(interp, "globals").Interface().(map[string]any)
	p.desc.Main, p.desc.Mode, p.desc.Builder = interp.MainPkg().Pkg.Path(), ctx.Mode, ctx.BuilderMode
	if ctx.MethodChecker != nil {
		name := runtime.FuncForPC(reflect.ValueOf(ctx.MethodChecker).Pointer()).Name()
		if name != "github.com/goplus/ixgo.defaultChecker" && name != "github.com/goplus/ixgo.runtimeChecker" {
			return nil, fmt.Errorf("ixgo context has a custom method checker")
		}
	}
	if ctx.IsEvalMode() || ctx.RunContext != nil || ctx.BuildContext.GOARCH != runtime.GOARCH || ctx.BuildContext.GOOS != runtime.GOOS {
		return nil, fmt.Errorf("ixgo context uses non-transferable execution configuration")
	}
	for _, field := range []string{"output", "debugFunc", "panicFunc", "evalCallFn"} {
		if !privateField(ctx, field).IsNil() {
			return nil, fmt.Errorf("ixgo context %s cannot be transferred", field)
		}
	}
	iter := privateField(interp, "funcs").MapRange()
	byName := make(map[string]*ixgoFunction)
	for iter.Next() {
		fn := iter.Key().Interface().(*ssa.Function)
		name := fn.String()
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("ambiguous ixgo function %s", name)
		}
		byName[name] = (*ixgoFunction)(iter.Value().UnsafePointer())
		p.desc.Functions = append(p.desc.Functions, name)
	}
	sort.Strings(p.desc.Functions)
	for _, name := range p.desc.Functions {
		fn := byName[name]
		p.functionIDs[fn.Fn] = len(p.functions)
		p.functions = append(p.functions, fn)
	}
	record := privateField(interp, "record").Interface().(*ixgo.TypesRecord)
	cache := privateField(record, "rcache").Interface().(map[reflect.Type]types.Type)
	for rt, typ := range cache {
		// ixgo includes local-declaration and instance identities in runtime
		// names. Keep both names, and reject ambiguity instead of guessing.
		key := types.TypeString(typ, func(p *types.Package) string { return p.Path() }) + "|" + rt.String()
		if old, exists := p.types[key]; exists && old != rt {
			return nil, fmt.Errorf("ambiguous ixgo type %s", key)
		}
		p.types[key], p.typeKeys[rt] = rt, key
	}
	overrides := (*sync.Map)(unsafe.Pointer(privateField(ctx, "override").UnsafeAddr()))
	overrides.Range(func(key, value any) bool {
		p.overrides[key.(string)] = value.(reflect.Value)
		return true
	})
	return p, nil
}

func (p *ixgoProgram) source() error {
	for _, pkg := range p.interp.MainPkg().Prog.AllPackages() {
		init := pkg.Func("init")
		if init == nil || init.Blocks == nil {
			continue
		}
		sp := p.ctx.SourcePackage(pkg.Pkg.Path())
		if sp == nil || len(sp.Files) == 0 {
			return fmt.Errorf("ixgo source unavailable for %s", pkg.Pkg.Path())
		}
		src := ixgoSource{Path: pkg.Pkg.Path()}
		for i, file := range sp.Files {
			var b bytes.Buffer
			if err := format.Node(&b, p.ctx.FileSet, file); err != nil {
				return err
			}
			src.Names = append(src.Names, fmt.Sprintf("%s/%d.go", pkg.Pkg.Path(), i))
			src.Files = append(src.Files, b.String())
		}
		p.desc.Sources = append(p.desc.Sources, src)
	}
	sort.Slice(p.desc.Sources, func(i, j int) bool { return p.desc.Sources[i].Path < p.desc.Sources[j].Path })
	return nil
}

// Discover every interpreter before encoding values: a Formula's reflect.Value
// field can appear before the hook that identifies its owning interpreter.
func (im *valueImage) discover(fn reflect.Value) error {
	seen := make(map[objectRef]bool)
	var visit func(reflect.Value, string, int) error
	visit = func(v reflect.Value, path string, depth int) error {
		if depth > 256 {
			return fmt.Errorf("%s: graph nesting exceeds 256", path)
		}
		v = addressable(v)
		if v.Type() == reflect.TypeFor[reflect.Value]() {
			rv := v.Interface().(reflect.Value)
			if !rv.IsValid() {
				return nil
			}
			if !rv.CanInterface() {
				return fmt.Errorf("%s: reflected value has restricted access", path)
			}
			if rv.CanAddr() {
				rv = rv.Addr()
			}
			return visit(rv, path+".Value", depth+1)
		}
		if err := transferable(v.Type()); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		switch v.Kind() {
		case reflect.Interface:
			if v.IsNil() {
				return nil
			}
			if _, ok := v.Elem().Interface().(reflect.Type); ok {
				return nil
			}
			return visit(v.Elem(), path+".(value)", depth+1)
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func:
			if v.IsNil() {
				return nil
			}
			key := objectRef{typ: v.Type(), addr: objectAddress(v)}
			if v.Kind() == reflect.Slice {
				key.span = v.Cap() + 1
			}
			if seen[key] {
				return nil
			}
			seen[key] = true
		}
		switch v.Kind() {
		case reflect.Pointer:
			if standardStream(v) != 0 {
				return nil
			}
			return visit(v.Elem(), path+"*", depth+1)
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if err := visit(v.Field(i), path+"."+v.Type().Field(i).Name, depth+1); err != nil {
					return err
				}
			}
		case reflect.Array, reflect.Slice:
			if v.Kind() == reflect.Slice {
				v = v.Slice(0, v.Cap())
			}
			for i := 0; i < v.Len(); i++ {
				if err := visit(v.Index(i), path+"[]", depth+1); err != nil {
					return err
				}
			}
		case reflect.Map:
			it := v.MapRange()
			for it.Next() {
				if err := visit(it.Key(), path+"{key}", depth+1); err != nil {
					return err
				}
				if err := visit(it.Value(), path+"{value}", depth+1); err != nil {
					return err
				}
			}
		case reflect.Func:
			c, err := im.metadata.ixgoCallback(v)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if c != nil {
				var p *ixgoProgram
				for _, candidate := range im.programs {
					if candidate.interp == c.interp {
						p = candidate
						break
					}
				}
				if p == nil {
					p, err = indexIxgo(c.interp)
					if err != nil {
						return err
					}
					if err := p.source(); err != nil {
						return err
					}
					im.programs = append(im.programs, p)
					for _, value := range p.globals {
						if err := visit(reflect.ValueOf(value), "global", depth+1); err != nil {
							return err
						}
					}
					for name, value := range p.overrides {
						if err := visit(value, "external."+name, depth+1); err != nil {
							return err
						}
					}
				}
				for i, value := range c.env {
					if value == nil {
						continue
					}
					if err := visit(reflect.ValueOf(value), fmt.Sprintf("%s.env[%d]", path, i), depth+1); err != nil {
						return err
					}
				}
				return nil
			}
			layout, err := im.metadata.layout(v.Pointer(), v.Type())
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			root := functionAddress(v)
			for i := 1; i < layout.storage.NumField(); i++ {
				f := layout.storage.Field(i)
				if f.Name[0] == 'P' {
					continue
				}
				if err := visit(reflect.NewAt(f.Type, unsafe.Pointer(root+f.Offset)).Elem(), path+"."+f.Name, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(fn, "fn", 0)
}

func (w *imageWriter) exportIxgo(v reflect.Value, c *ixgoCallback, path string, depth int) (uintptr, error) {
	ref := objectRef{typ: v.Type(), addr: functionAddress(v), span: -2}
	if old, ok := w.refs[ref]; ok {
		return old, nil
	}
	program := -1
	for i, p := range w.programs {
		if p.interp == c.interp {
			program = i
			break
		}
	}
	if program < 0 {
		return 0, fmt.Errorf("%s: ixgo interpreter was not discovered", path)
	}
	index, ok := w.programs[program].functionIDs[c.pfn.Fn]
	if !ok {
		return 0, fmt.Errorf("%s: ixgo function was not indexed", path)
	}
	header, err := w.alloc(40, 8)
	if err != nil {
		return 0, err
	}
	env, err := w.alloc(uintptr(len(c.env))*16, 8)
	if err != nil {
		return 0, err
	}
	w.refs[ref] = header
	w.retain(ref, v)
	w.put(header, ixgoImageTag)
	w.put(header+8, uintptr(program))
	w.put(header+16, uintptr(index))
	w.put(header+24, env)
	w.put(header+32, uintptr(len(c.env)))
	for i := range c.env {
		if err := w.copy(reflect.ValueOf(&c.env[i]).Elem(), env+uintptr(i)*16, fmt.Sprintf("%s.env[%d]", path, i), depth+1); err != nil {
			return 0, err
		}
	}
	return header, nil
}

func (r *imageReader) restoreIxgo(header uintptr, typ reflect.Type, path string, depth int) (reflect.Value, error) {
	ref := objectRef{typ: typ, addr: header, span: -2}
	if v, ok := r.refs[ref]; ok {
		return v, nil
	}
	if _, err := r.bytes(header, 40); err != nil {
		return reflect.Value{}, err
	}
	program, _ := r.word(header + 8)
	index, _ := r.word(header + 16)
	address, _ := r.word(header + 24)
	count, _ := r.word(header + 32)
	if program >= uintptr(len(r.programs)) || index >= uintptr(len(r.programs[program].functions)) {
		return reflect.Value{}, fmt.Errorf("%s: invalid ixgo function identity", path)
	}
	p := r.programs[program]
	pfn := p.functions[index]
	if count != uintptr(len(pfn.Fn.FreeVars)) || ixgoToType(p.interp, pfn.Fn.Type()) != typ {
		return reflect.Value{}, fmt.Errorf("%s: ixgo signature or capture count mismatch", path)
	}
	if err := r.span(address, count, reflect.TypeFor[any]()); err != nil {
		return reflect.Value{}, err
	}
	fn, bound := r.bindings[ref]
	env := make([]any, int(count))
	if bound {
		c, err := r.metadata.ixgoCallback(fn)
		if err != nil || c == nil || c.interp != p.interp || c.pfn.Fn != pfn.Fn {
			return reflect.Value{}, fmt.Errorf("%s: retained ixgo function changed identity", path)
		}
		env = c.env
	} else {
		fn = ixgoMakeFunction(pfn, typ, env)
	}
	r.refs[ref] = fn
	for i := range env {
		if err := r.copy(reflect.ValueOf(&env[i]).Elem(), address+uintptr(i)*16, fmt.Sprintf("%s.env[%d]", path, i), depth+1); err != nil {
			return reflect.Value{}, err
		}
		want := ixgoToType(p.interp, pfn.Fn.FreeVars[i].Type())
		if env[i] == nil || reflect.TypeOf(env[i]) != want {
			return reflect.Value{}, fmt.Errorf("%s.env[%d]: captured value does not match %s", path, i, want)
		}
	}
	return fn, nil
}

func (w *imageWriter) programValues() ([]ixgoDescription, error) {
	descriptions := make([]ixgoDescription, len(w.programs))
	for i, p := range w.programs {
		d := p.desc
		d.Globals, d.Overrides = nil, nil
		globals := make(map[string]reflect.Value, len(p.globals))
		for name, value := range p.globals {
			globals[name] = reflect.ValueOf(value)
		}
		for _, group := range []struct {
			values map[string]reflect.Value
			slots  *[]namedSlot
		}{
			{globals, &d.Globals},
			{p.overrides, &d.Overrides},
		} {
			names := make([]string, 0, len(group.values))
			for name := range group.values {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				v := group.values[name]
				id, err := w.typeID(v.Type())
				if err != nil {
					return nil, err
				}
				slot, err := w.alloc(v.Type().Size(), v.Type().Align())
				if err != nil {
					return nil, err
				}
				if err := w.copy(snapshot(v), slot, name, 0); err != nil {
					return nil, err
				}
				*group.slots = append(*group.slots, namedSlot{name, id, slot})
			}
		}
		descriptions[i] = d
	}
	return descriptions, nil
}

func (w *imageWriter) metadataDescription(programs []ixgoDescription) error {
	if len(programs) == 0 && len(w.typeDefs) == 0 {
		return nil
	}
	b, err := json.Marshal(transferDescription{programs, w.typeDefs})
	if err != nil {
		return err
	}
	slot, err := w.alloc(uintptr(len(b)), 1)
	if err != nil {
		return err
	}
	copy(w.mem[slot:], b)
	w.put(56, slot)
	w.put(64, uintptr(len(b)))
	return nil
}

func (im *valueImage) prepare(expected *valueImage) error {
	if err := im.header(); err != nil {
		return err
	}
	addr, size := headerWord(im.mem, 56), headerWord(im.mem, 64)
	if addr == 0 && size == 0 {
		if expected != nil && (len(expected.programs) != 0 || len(expected.typeDefs) != 0) {
			return fmt.Errorf("missing transfer metadata")
		}
		if expected != nil {
			return nil
		}
		return im.authorizeFunctions()
	}
	b, err := im.bytes(addr, size)
	if err != nil {
		return err
	}
	var description transferDescription
	if err := json.Unmarshal(b, &description); err != nil {
		return fmt.Errorf("transfer metadata: %w", err)
	}
	im.typeDefs = description.Types
	if expected != nil {
		if len(description.Programs) != len(expected.programs) || len(im.typeDefs) < len(expected.typeDefs) {
			return fmt.Errorf("changed transfer program or type count")
		}
		for i, d := range description.Programs {
			original := expected.programs[i].desc
			if d.Main != original.Main || d.Mode != original.Mode || d.Builder != original.Builder || !reflect.DeepEqual(d.Sources, original.Sources) || !reflect.DeepEqual(d.Functions, original.Functions) {
				return fmt.Errorf("guest changed ixgo program %d", i)
			}
		}
		for i, d := range expected.typeDefs {
			if !reflect.DeepEqual(d, im.typeDefs[i]) {
				return fmt.Errorf("guest changed transfer type %d", i)
			}
		}
		im.programs = expected.programs
		if err := im.resolveTypes(); err != nil {
			return err
		}
		return nil
	}
	// External bindings must exist before ixgo compiles calls to them. They are
	// decoded first; references to this program's not-yet-built types fail here.
	if err := im.authorizeFunctions(); err != nil {
		return err
	}
	r := &imageReader{valueImage: im, refs: make(map[objectRef]reflect.Value)}
	for _, d := range description.Programs {
		ctx := ixgo.NewContext(d.Mode | ixgo.SupportMultipleInterp)
		ctx.BuilderMode = d.Builder
		for _, ext := range d.Overrides {
			t := im.types[ext.Type]
			if t == nil {
				return fmt.Errorf("external %s requires an unavailable type", ext.Name)
			}
			v := reflect.New(t).Elem()
			if err := r.copy(v, ext.Slot, "external."+ext.Name, 0); err != nil {
				return err
			}
			ctx.RegisterExternal(ext.Name, v.Interface())
		}
		for _, src := range d.Sources {
			if len(src.Files) == 0 || len(src.Files) != len(src.Names) {
				return fmt.Errorf("invalid source package %s", src.Path)
			}
			if err := ctx.AddImportFile(src.Path, src.Names[0], src.Files[0]); err != nil {
				return err
			}
			sp := ctx.SourcePackage(src.Path)
			for i := 1; i < len(src.Files); i++ {
				file, err := ctx.ParseFile(src.Names[i], src.Files[i])
				if err != nil {
					return err
				}
				sp.Files = append(sp.Files, file)
			}
		}
		sp := ctx.SourcePackage(d.Main)
		if sp == nil {
			return fmt.Errorf("missing main ixgo source")
		}
		if err := sp.Load(); err != nil {
			return err
		}
		pkg, err := ixgoBuildMain(ctx, sp)
		if err != nil {
			return err
		}
		interp, err := ctx.NewInterp(pkg)
		if err != nil {
			return err
		}
		p, err := indexIxgo(interp)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(p.desc.Functions, d.Functions) {
			return fmt.Errorf("rebuilt ixgo function set differs")
		}
		p.desc = d
		im.programs = append(im.programs, p)
	}
	if err := im.resolveTypes(); err != nil {
		return err
	}
	im.initial = r.refs
	return nil
}

func (im *valueImage) globalBindings() (map[objectRef]reflect.Value, error) {
	bindings := make(map[objectRef]reflect.Value)
	for _, p := range im.programs {
		if len(p.desc.Globals) != len(p.globals) {
			return nil, fmt.Errorf("ixgo global count differs")
		}
		seen := make(map[string]bool)
		for _, g := range p.desc.Globals {
			v := reflect.ValueOf(p.globals[g.Name])
			if !v.IsValid() || seen[g.Name] || v.Type() != im.types[g.Type] {
				return nil, fmt.Errorf("invalid ixgo global %s", g.Name)
			}
			seen[g.Name] = true
			key, err := im.bindingKey(v.Type(), g.Slot)
			if err != nil {
				return nil, err
			}
			if old, ok := bindings[key]; ok && objectAddress(old) != objectAddress(v) {
				return nil, fmt.Errorf("ixgo globals were merged")
			}
			bindings[key] = v
		}
	}
	return bindings, nil
}
