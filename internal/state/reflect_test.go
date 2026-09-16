package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"unsafe"

	"github.com/xgo-dev/sandbox/internal/reflecttype"
)

type reflectedNamed struct{ Value int }

func (v reflectedNamed) Number() int { return v.Value }

type reflectedInterface interface{ Number() int }

func TestReflectedTypeValues(t *testing.T) {
	item := reflect.StructOf([]reflect.StructField{
		{Name: "Count", Type: reflect.TypeFor[int](), Tag: `json:"count"`},
		{Name: "hidden", PkgPath: "state_test", Type: reflect.TypeFor[string]()},
	})
	integer := reflect.TypeFor[int]()
	list := reflect.SliceOf(item)
	src := []reflect.Type{
		nil, integer, reflect.TypeFor[unsafe.Pointer](), item, reflect.PointerTo(item), list,
		reflect.ArrayOf(3, item), reflect.MapOf(reflect.TypeFor[string](), item),
		reflect.ChanOf(reflect.BothDir, item), reflect.ChanOf(reflect.SendDir, item), reflect.ChanOf(reflect.RecvDir, item),
		reflect.FuncOf([]reflect.Type{integer, list}, []reflect.Type{reflect.TypeFor[error]()}, true),
		reflect.TypeFor[reflectedNamed](), reflect.TypeFor[reflectedInterface](), reflect.TypeFor[any](),
	}
	var dst []reflect.Type
	roundtrip(t, &src, &dst)
	if !reflect.DeepEqual(src, dst) {
		t.Fatalf("types changed: got %v, want %v", dst, src)
	}
	if dst[len(dst)-2].NumMethod() != 1 {
		t.Fatal("nonempty interface type lost its method set")
	}
	// A reflect.Type may share an interface column with ordinary values.
	mixed := []any{item, 42, nil, integer, "text", reflectedNamed{Value: 7}}
	var copied []any
	roundtrip(t, &mixed, &copied)
	if !reflect.DeepEqual(copied, mixed) {
		t.Fatalf("mixed interfaces changed: %#v", copied)
	}
	if copied[5].(reflectedInterface).Number() != 7 {
		t.Fatal("named value lost its methods")
	}
	keys := map[reflect.Type]any{item: integer, integer: 42, list: nil}
	var copiedKeys map[reflect.Type]any
	roundtrip(t, &keys, &copiedKeys)
	if !reflect.DeepEqual(copiedKeys, keys) {
		t.Fatalf("reflect.Type map keys changed: %#v", copiedKeys)
	}
}

func TestReflectedDynamicGraph(t *testing.T) {
	typ := reflect.StructOf([]reflect.StructField{
		{Name: "Count", Type: reflect.TypeFor[int](), Tag: `state:"dynamic"`},
		{Name: "Items", Type: reflect.TypeFor[[]string]()},
	})
	value := reflect.New(typ)
	value.Elem().Field(0).SetInt(42)
	value.Elem().Field(1).Set(reflect.ValueOf([]string{"host"}))
	field := value.Elem().Field(0).Addr().Interface().(*int)
	type root struct {
		Field  *int
		Type   reflect.Type
		Value  any
		Nil    any
		Custom *graphNode
	}
	src := root{Field: field, Type: typ, Value: value.Interface(), Nil: reflect.Zero(value.Type()).Interface(), Custom: &graphNode{Value: 9}}
	var dst root
	roundtrip(t, &src, &dst)
	restored := reflect.ValueOf(dst.Value).Elem()
	if dst.Type != typ || restored.Type() != typ || dst.Field != restored.Field(0).Addr().Interface().(*int) {
		t.Fatal("dynamic type or interior pointer identity changed")
	}
	if reflect.TypeOf(dst.Nil) != value.Type() || !reflect.ValueOf(dst.Nil).IsNil() {
		t.Fatal("typed nil lost its dynamic type")
	}
	if dst.Custom.Value != 9 || !dst.Custom.loaded {
		t.Fatal("custom StateLoad was not preserved")
	}
	*dst.Field = 99
	if restored.Field(0).Int() != 99 || *field != 42 {
		t.Fatal("restored storage does not preserve aliases and isolation")
	}
}

type lateReflectedType struct{ Type reflect.Type }

func (*lateReflectedType) StateTypeName() string { return "state.test.lateReflectedType" }
func (*lateReflectedType) StateFields() []string { return []string{"Type"} }
func (*lateReflectedType) StateSave(s Sink) {
	typ := reflect.StructOf([]reflect.StructField{{Name: "CreatedDuringSave", Type: reflect.TypeFor[int]()}})
	s.SaveValue(0, typ)
}
func (v *lateReflectedType) StateLoad(_ context.Context, s Source) { s.Load(0, &v.Type) }

func init() { Register((*lateReflectedType)(nil)) }

func TestTypeCreatedDuringStateSave(t *testing.T) {
	var src, dst lateReflectedType
	roundtrip(t, &src, &dst)
	if dst.Type == nil || dst.Type.Field(0).Name != "CreatedDuringSave" {
		t.Fatalf("type created during Save was omitted: %v", dst.Type)
	}
}

type reflectedProcessRoot struct {
	Type  reflect.Type
	Value any
}

func TestReflectedTypesNewProcess(t *testing.T) {
	const imageEnv = "SANDBOX_STATE_REFLECT_TEST_IMAGE"
	if path := os.Getenv(imageEnv); path != "" {
		mem, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var root reflectedProcessRoot
		if _, err := Load(context.Background(), mem, &root); err != nil {
			t.Fatal(err)
		}
		value := reflect.ValueOf(root.Value).Elem()
		if root.Type != value.Type() || value.Field(0).Int() != 42 {
			t.Fatal("type and value did not arrive together")
		}
		value.Field(0).SetInt(43)
		// The return graph contains a type that did not exist during host Save.
		root.Type = reflect.MapOf(reflect.TypeFor[string](), root.Type)
		output := make([]byte, 1<<20)
		n, _, err := Save(context.Background(), output, &root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".out", output[:n], 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	typ := reflect.StructOf([]reflect.StructField{{Name: "Count", Type: reflect.TypeFor[int](), Tag: `state:"process"`}})
	value := reflect.New(typ)
	value.Elem().Field(0).SetInt(42)
	src := reflectedProcessRoot{Type: typ, Value: value.Interface()}
	mem := make([]byte, 1<<20)
	n, _, err := Save(context.Background(), mem, &src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "reflect.state")
	if err := os.WriteFile(path, mem[:n], 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReflectedTypesNewProcess$")
	cmd.Env = append(os.Environ(), imageEnv+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}
	output, err := os.ReadFile(path + ".out")
	if err != nil {
		t.Fatal(err)
	}
	var dst reflectedProcessRoot
	if _, err := Load(context.Background(), output, &dst); err != nil {
		t.Fatal(err)
	}
	if dst.Type.Kind() != reflect.Map || dst.Type.Elem() != typ || reflect.ValueOf(dst.Value).Elem().Field(0).Int() != 43 {
		t.Fatal("return transfer lost new types or changed values")
	}
	if value.Elem().Field(0).Int() != 42 {
		t.Fatal("Load unexpectedly modified the original graph")
	}
}

func TestReflectedTypeIDsInStream(t *testing.T) {
	src := reflect.StructOf([]reflect.StructField{{Name: "SnapshotID", Type: reflect.TypeFor[int]()}})
	mem := make([]byte, 1<<20)
	n, _, err := Save(context.Background(), mem, &src)
	if err != nil {
		t.Fatal(err)
	}
	r := reader{mem: mem[:n]}
	length, objects, err := readHeader(&r)
	if err != nil || objects || length == 0 {
		t.Fatalf("missing type table: length=%d objects=%v error=%v", length, objects, err)
	}
	types, err := reflecttype.Open(r.readBytes(length))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := readHeader(&r); err != nil {
		t.Fatal(err)
	}
	if _, err := r.get(); err != nil { // Root object ID.
		t.Fatal(err)
	}
	obj, err := r.get()
	if err != nil {
		t.Fatal(err)
	}
	ref := obj.(*interfaceValue).Value.(*reflectTypeValue).Type.(*reflectedType)
	got, err := types.Resolve(uint32(ref.ID))
	if err != nil || got != src {
		t.Fatalf("state and reflecttype disagree on ID %d: %v, %v", ref.ID, got, err)
	}
	// Re-encode the object with an out-of-range ID; Load must not truncate it.
	ref.ID = 1 << 32
	w := writer{mem: make([]byte, len(mem))}
	// Rebuild the prelude explicitly, independent of object byte lengths.
	tableReader := reader{mem: mem[:n]}
	tableLength, _, _ := readHeader(&tableReader)
	table := tableReader.readBytes(tableLength)
	if err := writeHeader(&w, uint64(len(table)), false); err != nil {
		t.Fatal(err)
	}
	w.writeBytes(table)
	if err := writeHeader(&w, 1, true); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []object{uintValue(1), obj} {
		if err := w.put(entry); err != nil {
			t.Fatal(err)
		}
	}
	var dst reflect.Type
	if _, err := Load(context.Background(), w.mem[:w.pos], &dst); err == nil {
		t.Fatal("accepted overflowing reflected type ID")
	}
}
