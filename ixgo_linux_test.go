//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"encoding/binary"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/goplus/ixgo"
	_ "github.com/goplus/ixgo/pkg/reflect"
)

const ixgoTestSource = `package main
import "reflect"
var initialized = 1
var total = 100
type Node struct { Value int; Next *Node }
func (n *Node) Read() int { return n.Value }
type Reader interface { Read() int }
func Native(n int) int { panic("external was not restored") }
func Make(n *int) (func() int, func() int) {
    p := &Node{Value: 20}
    p.Next = p
    xs := []*Node{p}
    m := map[string]any{"node":p}
    var boxed any = p
    var reader Reader = p
    step := 2
    return func() int {
        if initialized != 1 || p.Next != p || xs[0] != p || m["node"] != p || boxed != p { panic("lost graph") }
        step++; *n += step; p.Value++; total++
        return Native(reader.Read()) + total
    }, func() int { step++; *n += step; return p.Value + total }
}
func MakeNext() func() func() int {
    n := 10
    return func() func() int { n++; return func() int { n++; return n } }
}
func MakeReflect(n *int) func() int {
    v := reflect.ValueOf(n).Elem()
    t := reflect.TypeOf(n)
    return func() int { if t.Kind() != reflect.Ptr { panic("type") }; v.SetInt(v.Int()+1); return *n }
}
type Demo struct { Node; Hook func() int }
func (d *Demo) Setup() { d.Hook = func() int { d.Value++; return d.Value } }
`

func ixgoNative(n int) int { return n + 1 }

func testInterpreter(t *testing.T) *ixgo.Interp {
	t.Helper()
	ctx := ixgo.NewContext(ixgo.SupportMultipleInterp)
	ctx.RegisterExternal("main.Native", ixgoNative)
	i, err := ctx.LoadInterp("program.go", ixgoTestSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.RunInit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.UnsafeRelease)
	return i
}

// A separate metadata/type registry models the two runtimes. No host dynamic
// type addresses or interpreter pointers are provided to the receiving image.
func ixgoRoundtrip(t *testing.T, fn func(), corrupt func(*valueImage)) error {
	t.Helper()
	m, err := loadMetadata()
	if err != nil {
		t.Fatal(err)
	}
	in := newImage(make([]byte, imageBytes), m, make(map[uintptr]nativeLayout))
	w, err := in.encode(reflect.ValueOf(fn), nil)
	if err != nil {
		return err
	}
	guest := newImage(append([]byte(nil), in.mem...), m, make(map[uintptr]nativeLayout))
	if err := guest.prepare(nil); err != nil {
		return err
	}
	for _, p := range guest.programs {
		t.Cleanup(p.interp.UnsafeRelease)
	}
	bindings, err := guest.globalBindings()
	if err != nil {
		return err
	}
	gfn, retained, err := guest.decode(bindings)
	if err != nil {
		return err
	}
	gfn.Interface().(func())()
	out := newImage(make([]byte, imageBytes), m, guest.native)
	out.inherit(guest)
	if _, err := out.encode(gfn, retained); err != nil {
		return err
	}
	if corrupt != nil {
		corrupt(out)
	}
	result := newImage(append([]byte(nil), out.mem...), m, in.native)
	if err := result.prepare(in); err != nil {
		return err
	}
	return result.commit(w.anchors)
}

func TestIxgoSharedEnvironment(t *testing.T) {
	i := testInterpreter(t)
	n := 10
	value, err := i.RunFunc("Make", &n)
	if err != nil {
		t.Fatal(err)
	}
	pair := value.(ixgo.Tuple)
	a, b := pair[0].(func() int), pair[1].(func() int)
	var first, second int
	if err := ixgoRoundtrip(t, func() { first = a(); second = b() }, nil); err != nil {
		t.Fatal(err)
	}
	global, _ := i.GetVarAddr("total")
	if n != 17 || first != 123 || second != 122 || *global.(*int) != 101 {
		t.Fatalf("n=%d first=%d second=%d global=%v", n, first, second, *global.(*int))
	}
	if got := b(); got != 122 || n != 22 {
		t.Fatalf("original closure: got=%d n=%d", got, n)
	}
}

func TestIxgoNewClosure(t *testing.T) {
	i := testInterpreter(t)
	v, err := i.RunFunc("MakeNext")
	if err != nil {
		t.Fatal(err)
	}
	makeNext := v.(func() func() int)
	var next func() int
	if err := ixgoRoundtrip(t, func() { next = makeNext() }, nil); err != nil {
		t.Fatal(err)
	}
	if got := next(); got != 12 {
		t.Fatalf("returned closure: %d", got)
	}
	if got := makeNext()(); got != 14 {
		t.Fatalf("returned closure lost shared cell: %d", got)
	}
}

func TestIxgoReflectedCaptures(t *testing.T) {
	i := testInterpreter(t)
	n := 10
	v, err := i.RunFunc("MakeReflect", &n)
	if err != nil {
		t.Fatal(err)
	}
	fn := v.(func() int)
	var result int
	if err := ixgoRoundtrip(t, func() { result = fn() }, nil); err != nil {
		t.Fatal(err)
	}
	if n != 11 || result != 11 {
		t.Fatalf("n=%d result=%d", n, result)
	}
}

func TestIxgoClassStateBeforeHook(t *testing.T) {
	i := testInterpreter(t)
	typ, ok := i.GetType("Demo")
	if !ok {
		t.Fatal("missing Demo")
	}
	v := reflect.New(typ)
	v.Interface().(interface{ Setup() }).Setup()
	v.Elem().FieldByName("Value").SetInt(40)
	holder := &struct {
		Value reflect.Value
		Type  reflect.Type
		Hook  func() int
	}{v.Elem(), typ, v.Elem().FieldByName("Hook").Interface().(func() int)}
	var result int
	if err := ixgoRoundtrip(t, func() { result = holder.Hook() }, nil); err != nil {
		t.Fatal(err)
	}
	if result != 41 || v.Elem().FieldByName("Value").Int() != 41 || holder.Type != typ {
		t.Fatalf("class result=%d value=%v", result, v.Elem())
	}
}

func TestIxgoRejectedOutputDoesNotCommit(t *testing.T) {
	for _, corruption := range []string{"function", "capture-type", "metadata"} {
		t.Run(corruption, func(t *testing.T) {
			i := testInterpreter(t)
			n := 10
			v, err := i.RunFunc("Make", &n)
			if err != nil {
				t.Fatal(err)
			}
			fn := v.(ixgo.Tuple)[0].(func() int)
			result := 0
			corrupted := false
			err = ixgoRoundtrip(t, func() { result = fn() }, func(out *valueImage) {
				corrupted = true
				if corruption == "metadata" {
					binary.LittleEndian.PutUint64(out.mem[64:], uint64(len(out.mem)))
					return
				}
				for addr := uintptr(4096); addr+40 < out.used; addr += 8 {
					tag, _ := out.word(addr)
					if tag != ixgoImageTag {
						continue
					}
					if corruption == "function" {
						binary.LittleEndian.PutUint64(out.mem[addr+16:], ^uint64(0))
					} else {
						env, _ := out.word(addr + 24)
						binary.LittleEndian.PutUint64(out.mem[env:], uint64(typeAddress(reflect.TypeFor[int]())))
					}
					return
				}
				t.Fatal("ixgo record missing")
			})
			if !corrupted {
				t.Fatalf("failed before output corruption: %v", err)
			}
			if err == nil {
				t.Fatal("corrupt output accepted")
			}
			global, _ := i.GetVarAddr("total")
			if n != 10 || result != 0 || *global.(*int) != 100 {
				t.Fatalf("partial mutation: n=%d result=%d global=%v error=%v", n, result, global, err)
			}
		})
	}
}

func TestNonIxgoMakeFuncRejected(t *testing.T) {
	fn := reflect.MakeFunc(reflect.TypeFor[func()](), func([]reflect.Value) []reflect.Value { return nil }).Interface().(func())
	err := ixgoRoundtrip(t, fn, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported reflect.MakeFunc") {
		t.Fatalf("got %v", err)
	}
}

func TestIxgoTwoInterpreters(t *testing.T) {
	a, b := testInterpreter(t), testInterpreter(t)
	x, y := 10, 20
	av, err := a.RunFunc("Make", &x)
	if err != nil {
		t.Fatal(err)
	}
	bv, err := b.RunFunc("Make", &y)
	if err != nil {
		t.Fatal(err)
	}
	af, bf := av.(ixgo.Tuple)[0].(func() int), bv.(ixgo.Tuple)[0].(func() int)
	var ar, br int
	if err := ixgoRoundtrip(t, func() { ar = af(); br = bf() }, nil); err != nil {
		t.Fatal(err)
	}
	if x != 13 || y != 23 || ar != 123 || br != 123 {
		t.Fatalf("x=%d y=%d ar=%d br=%d", x, y, ar, br)
	}
}

func TestIxgoSourceDependency(t *testing.T) {
	ctx := ixgo.NewContext(ixgo.SupportMultipleInterp)
	if err := ctx.AddImportFile("example/dependency", "dependency.go", `package dependency
var Count = 7
type Node struct { Value int }
func Add(n *Node) int { Count++; n.Value++; return n.Value+Count }
`); err != nil {
		t.Fatal(err)
	}
	i, err := ctx.LoadInterp("main.go", `package main
import "example/dependency"
func Make() func() int { n := &dependency.Node{Value:10}; return func() int { return dependency.Add(n) } }
`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.UnsafeRelease)
	if err := i.RunInit(); err != nil {
		t.Fatal(err)
	}
	v, err := i.RunFunc("Make")
	if err != nil {
		t.Fatal(err)
	}
	fn := v.(func() int)
	var result int
	if err := ixgoRoundtrip(t, func() { result = fn() }, nil); err != nil {
		t.Fatal(err)
	}
	if result != 19 || fn() != 21 {
		t.Fatalf("dependency result=%d", result)
	}
}

func TestIxgoLocalGenericType(t *testing.T) {
	ctx := ixgo.NewContext(ixgo.SupportMultipleInterp)
	i, err := ctx.LoadInterp("main.go", `package main
func Make[T ~int](initial T) func() int {
    type Local struct { Value T }
    p := &Local{initial}
    return func() int { p.Value++; return int(p.Value) }
}
func MakeInt() func() int { return Make(10) }
`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.UnsafeRelease)
	if err := i.RunInit(); err != nil {
		t.Fatal(err)
	}
	v, err := i.RunFunc("MakeInt")
	if err != nil {
		t.Fatal(err)
	}
	fn := v.(func() int)
	var result int
	if err := ixgoRoundtrip(t, func() { result = fn() }, nil); err != nil {
		t.Fatal(err)
	}
	if result != 11 || fn() != 12 {
		t.Fatalf("generic result=%d", result)
	}
}

func TestIxgoExternalClosureAliases(t *testing.T) {
	ctx := ixgo.NewContext(ixgo.SupportMultipleInterp)
	n := 10
	ctx.RegisterExternal("main.Next", func() int { n++; return n })
	ctx.RegisterExternal("main.Alias", &n)
	i, err := ctx.LoadInterp("main.go", `package main
var Alias int
func Next() int { panic("unbound") }
func Make() func() int { return func() int { n := Next(); if Alias != n { panic("alias") }; return n } }
`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.UnsafeRelease)
	if err := i.RunInit(); err != nil {
		t.Fatal(err)
	}
	v, err := i.RunFunc("Make")
	if err != nil {
		t.Fatal(err)
	}
	fn := v.(func() int)
	result := 0
	if err := ixgoRoundtrip(t, func() { result = fn() }, nil); err != nil {
		t.Fatal(err)
	}
	if result != 11 || n != 11 || fn() != 12 {
		t.Fatalf("result=%d n=%d", result, n)
	}
}

func TestStandardStreamReferences(t *testing.T) {
	streams := []*os.File{os.Stdin, os.Stdout, os.Stderr}
	if err := ixgoRoundtrip(t, func() {
		if streams[0] != os.Stdin || streams[1] != os.Stdout || streams[2] != os.Stderr {
			panic("standard stream identity")
		}
	}, nil); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "ordinary-file")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = ixgoRoundtrip(t, func() { _ = f.Name() }, nil)
	if err == nil || !strings.Contains(err.Error(), "process-local state") {
		t.Fatalf("ordinary file: %v", err)
	}
}

func TestReflectedInterfaceValue(t *testing.T) {
	for _, value := range []any{nil, 42} {
		holder := &struct {
			Value reflect.Value
			Want  any
		}{reflect.ValueOf(struct{ Value any }{value}).Field(0), value}
		if err := ixgoRoundtrip(t, func() {
			v := holder.Value
			if !v.IsValid() || v.Kind() != reflect.Interface || v.CanAddr() || v.CanSet() {
				panic("reflected interface shape changed")
			}
			if v.Interface() != holder.Want {
				panic("reflected interface value changed")
			}
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIxgoMetadataTypeLimits(t *testing.T) {
	for _, corrupt := range []string{"recursive", "oversized"} {
		t.Run(corrupt, func(t *testing.T) {
			n := 10
			err := ixgoRoundtrip(t, func() { n++ }, func(out *valueImage) {
				d := transferType{Program: -1, Kind: reflect.Array, Elem: typeAddress(reflect.TypeFor[int]()), Length: imageBytes}
				if corrupt == "recursive" {
					d.Elem = dynamicTypeBit | 1
					d.Length = 1
				}
				out.typeDefs = []transferType{d}
				w := &imageWriter{valueImage: out}
				if err := w.metadataDescription(nil); err != nil {
					t.Fatal(err)
				}
				w.put(8, w.used)
			})
			if err == nil || n != 10 {
				t.Fatalf("error=%v n=%d", err, n)
			}
		})
	}
}
