package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/goplus/reflectx"
	"github.com/visualfc/xtype"
	"github.com/xgo-dev/sandbox/internal/reflecttype"
	"github.com/xgo-dev/sandbox/internal/reflectxtype"
)

type reflectxRoot struct {
	Type      reflect.Type
	Pointer   reflect.Type
	Handle    xtype.Type
	NilHandle xtype.Type
	Handles   [3]xtype.Type
	ByHandle  map[xtype.Type]reflect.Type
	Object    any
	NilObject any
	Field     *int
	Value     reflect.Value
	Whole     reflect.Value
	Keys      map[reflect.Type]any
}

func reflectxFixture() reflectxRoot {
	ctx := reflectx.NewContext()
	typ := reflectx.NamedTypeOf("example/state", "Node", reflect.TypeFor[struct {
		N    int
		Next *int
	}]())
	ptr := reflectx.PtrTo(typ)
	reflectx.SetUnderlying(typ, ctx.StructOf([]reflect.StructField{
		{Name: "N", Type: reflect.TypeFor[int]()}, {Name: "Next", Type: ptr},
	}))
	value := reflect.New(typ)
	value.Elem().Field(0).SetInt(42)
	value.Elem().Field(1).Set(value)
	handle := xtype.TypeOfType(typ)
	return reflectxRoot{
		Type: typ, Pointer: ptr, Handle: handle,
		Handles:  [3]xtype.Type{nil, handle, xtype.TypeOfType(reflect.TypeFor[int]())},
		ByHandle: map[xtype.Type]reflect.Type{nil: nil, handle: typ},
		Object:   value.Interface(), NilObject: reflect.Zero(ptr).Interface(),
		Field: value.Elem().Field(0).Addr().Interface().(*int),
		Value: value.Elem().Field(0),
		Whole: value.Elem(),
		Keys:  map[reflect.Type]any{typ: value.Interface(), reflect.TypeFor[int](): 7},
	}
}

func checkReflectxRoot(t *testing.T, got reflectxRoot, number int) {
	t.Helper()
	value := reflect.ValueOf(got.Object)
	if got.Type.Name() != "Node" || got.Type.PkgPath() != "example/state" || value.Type() != got.Pointer || value.Type().Elem() != got.Type {
		t.Fatal("dynamic type identity was not preserved")
	}
	if got.Handle != xtype.TypeOfType(got.Type) || got.NilHandle != nil || got.Handles != [3]xtype.Type{nil, got.Handle, xtype.TypeOfType(reflect.TypeFor[int]())} {
		t.Fatal("xtype handles were not rebuilt")
	}
	if got.ByHandle[got.Handle] != got.Type || got.ByHandle[nil] != nil || len(got.ByHandle) != 2 {
		t.Fatal("xtype map keys differ from represented types")
	}
	if value.Pointer() != value.Elem().Field(1).Pointer() || got.Field != value.Elem().Field(0).Addr().Interface().(*int) {
		t.Fatal("object cycle or interior pointer was lost")
	}
	if *got.Field != number || !got.Value.CanAddr() || got.Value.Addr().Interface().(*int) != got.Field {
		t.Fatal("reflect.Value did not retain its alias")
	}
	if !got.Whole.CanAddr() || got.Whole.Type() != got.Type || got.Whole.Addr().Pointer() != value.Pointer() {
		t.Fatal("dynamic reflect.Value did not retain its type or storage")
	}
	if reflect.TypeOf(got.NilObject) != got.Pointer || !reflect.ValueOf(got.NilObject).IsNil() || got.Keys[got.Type] != got.Object || got.Keys[reflect.TypeFor[int]()] != 7 {
		t.Fatal("typed nil or reflected type key changed")
	}
	runtime.GC()
	if value.Elem().Field(1).Elem().Field(0).Int() != int64(number) {
		t.Fatal("GC lost reconstructed value")
	}
}

func TestReflectxStateGraph(t *testing.T) {
	src := reflectxFixture()
	var dst reflectxRoot
	roundtrip(t, &src, &dst)
	checkReflectxRoot(t, dst, 42)
	if src.Type == dst.Type || src.Handle == dst.Handle || src.Field == dst.Field {
		t.Fatal("host identity or storage leaked into reconstructed graph")
	}
	*dst.Field = 99
	checkReflectxRoot(t, dst, 99)
	if *src.Field != 42 {
		t.Fatal("source was modified")
	}
	var again reflectxRoot
	roundtrip(t, &dst, &again)
	checkReflectxRoot(t, again, 99)
}

func TestReflectxStateTables(t *testing.T) {
	src := reflectxFixture()
	standard, err := reflecttype.Export()
	if err != nil {
		t.Fatal(err)
	}
	if standard.IDs[src.Type] != 0 || standard.IDs[src.Pointer] != 0 || standard.IDs[reflect.SliceOf(src.Type)] != 0 {
		t.Fatal("standard table accepted a reflectx dependency")
	}
	values := []reflect.Type{reflect.TypeFor[int](), src.Type, src.Type, src.Pointer}
	mem := make([]byte, 1<<20)
	n, _, err := Save(context.Background(), mem, &values)
	if err != nil {
		t.Fatal(err)
	}
	r := reader{mem: mem[:n]}
	length, objects, err := readHeader(&r)
	if err != nil || objects {
		t.Fatalf("standard header: %v", err)
	}
	native, err := reflecttype.Open(r.readBytes(length))
	if err != nil {
		t.Fatal(err)
	}
	length, objects, err = readHeader(&r)
	if err != nil || objects || length == 0 {
		t.Fatalf("reflectx header: %v", err)
	}
	extended, err := reflectxtype.Open(r.readBytes(length))
	if err != nil {
		t.Fatal(err)
	}
	count, objects, err := readHeader(&r)
	if err != nil || !objects {
		t.Fatalf("objects: %v", err)
	}
	for range count {
		if _, err := r.get(); err != nil {
			t.Fatal(err)
		}
		obj, err := r.get()
		if err != nil {
			t.Fatal(err)
		}
		array, ok := obj.(*arrayValue)
		if !ok {
			continue
		}
		for i, object := range array.Contents {
			ref := object.(*interfaceValue).Value.(*reflectTypeValue).Type.(*reflectedType)
			if ref.reflectx != (i != 0) {
				t.Fatalf("type %v routed to wrong table", values[i])
			}
			var typ reflect.Type
			if ref.reflectx {
				typ, err = extended.Resolve(uint32(ref.ID))
			} else {
				typ, err = native.Resolve(uint32(ref.ID))
			}
			if err != nil || typ.Name() != values[i].Name() || typ.Kind() != values[i].Kind() {
				t.Fatalf("reference %d: %v %v", ref.ID, typ, err)
			}
		}
		return
	}
	t.Fatal("represented type array missing")
}

type reflectxEmbedded int

func TestReflectxStateInterfaceAndFields(t *testing.T) {
	ctx := reflectx.NewContext()
	iface := ctx.InterfaceOf(nil, []reflect.Method{{Name: "Number", Type: reflect.TypeFor[func() int]()}})
	value := reflect.New(iface).Elem()
	value.Set(reflect.ValueOf(reflectedNamed{Value: 23}))
	strct := ctx.StructOf([]reflect.StructField{
		{Name: "reflectxEmbedded", PkgPath: "github.com/xgo-dev/sandbox/internal/state", Anonymous: true, Type: reflect.TypeFor[reflectxEmbedded]()},
		{Name: "_", PkgPath: "github.com/xgo-dev/sandbox/internal/state", Type: reflect.TypeFor[int]()},
		{Name: "_", PkgPath: "github.com/xgo-dev/sandbox/internal/state", Type: reflect.TypeFor[int]()},
	})
	object := reflect.New(strct)
	reflectValueRWAddr(object.Elem().Field(0)).Elem().SetInt(11)
	src := struct {
		Type      reflect.Type
		Interface reflect.Value
		Fields    any
	}{iface, value, object.Interface()}
	var dst struct {
		Type      reflect.Type
		Interface reflect.Value
		Fields    any
	}
	roundtrip(t, &src, &dst)
	if dst.Interface.Type() != dst.Type || dst.Interface.Interface().(interface{ Number() int }).Number() != 23 {
		t.Fatal("dynamic interface lost its signature or compiled implementation")
	}
	fields := reflect.ValueOf(dst.Fields).Elem()
	if fields.Field(0).Int() != 11 || !fields.Type().Field(0).Anonymous || fields.Type().Field(0).PkgPath != strct.Field(0).PkgPath || fields.Type().Field(1).Name != "_" || fields.Type().Field(2).Name != "_" {
		t.Fatal("reflectx struct fields were not preserved")
	}
}

func TestReflectxStateInvalidReference(t *testing.T) {
	src := reflectxFixture().Type
	reflect.SliceOf(src)
	snapshot, err := reflectxtype.Export()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		id   uintValue
		want string
	}{
		{"missing table", nil, 1, "reflectx type table missing"},
		{"zero ID", snapshot.Data, 0, "unknown reflectx type ID"},
		{"unknown ID", snapshot.Data, uintValue(len(snapshot.IDs) + 1), "unknown reflectx type ID"},
		{"overflow ID", snapshot.Data, 1 << 32, "invalid reflect type reference"},
		{"corrupt table", []byte{1}, 1, "import reflectx types"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := writer{mem: make([]byte, 1<<20)}
			if err := writeHeader(&w, 0, false); err != nil {
				t.Fatal(err)
			}
			if err := writeHeader(&w, uint64(len(tc.data)), false); err != nil {
				t.Fatal(err)
			}
			w.writeBytes(tc.data)
			if err := writeHeader(&w, 1, true); err != nil {
				t.Fatal(err)
			}
			for _, obj := range []object{uintValue(1), &reflectTypeValue{Type: &reflectedType{ID: tc.id, reflectx: true}}} {
				if err := w.put(obj); err != nil {
					t.Fatal(err)
				}
			}
			var dst reflect.Type
			if _, err := Load(context.Background(), w.mem[:w.pos], &dst); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load: %v; want %s", err, tc.want)
			}
		})
	}
}

func TestReflectxStateNewProcess(t *testing.T) {
	const childEnv = "SANDBOX_STATE_REFLECTX_IMAGE"
	if path := os.Getenv(childEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var root reflectxRoot
		if _, err := Load(context.Background(), data, &root); err != nil {
			t.Fatal(err)
		}
		checkReflectxRoot(t, root, 42)
		*root.Field = 43
		mem := make([]byte, 1<<20)
		n, _, err := Save(context.Background(), mem, &root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".out", mem[:n], 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	src := reflectxFixture()
	mem := make([]byte, 1<<20)
	n, _, err := Save(context.Background(), mem, &src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.bin")
	if err := os.WriteFile(path, mem[:n], 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReflectxStateNewProcess$")
	cmd.Env = append(os.Environ(), childEnv+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guest: %v\n%s", err, output)
	}
	data, err := os.ReadFile(path + ".out")
	if err != nil {
		t.Fatal(err)
	}
	var dst reflectxRoot
	if _, err := Load(context.Background(), data, &dst); err != nil {
		t.Fatal(err)
	}
	checkReflectxRoot(t, dst, 43)
	if *src.Field != 42 {
		t.Fatal("guest changed original storage")
	}
}

func TestReflectxStateRejectMethods(t *testing.T) {
	const childEnv = "SANDBOX_STATE_REFLECTX_METHOD"
	if mode := os.Getenv(childEnv); mode != "" {
		base := reflect.TypeFor[struct{ N int }]()
		if mode == "named" {
			base = reflectx.NamedTypeOf("example/method", "T", base)
		}
		typ := reflectx.NewMethodSet(base, 1, 1)
		m := reflectx.MakeMethod("hidden", "example/method", true, reflect.TypeFor[func()](), func([]reflect.Value) []reflect.Value { return nil })
		if err := reflectx.SetMethodSet(typ, []reflectx.Method{m}, false); err != nil {
			t.Fatal(err)
		}
		_, _, err := Save(context.Background(), make([]byte, 1<<20), &typ)
		if err == nil || !strings.Contains(err.Error(), "concrete method implementations") {
			t.Fatalf("unsupported method: %v", err)
		}
		return
	}
	for _, mode := range []string{"named", "unnamed"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestReflectxStateRejectMethods$")
		cmd.Env = append(os.Environ(), childEnv+"="+mode)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, output)
		}
	}
}
