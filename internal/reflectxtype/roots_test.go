package reflectxtype

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/goplus/reflectx"
)

func TestExportRoots(t *testing.T) {
	dependency := reflectx.NamedTypeOf("example/roots", "Value", reflect.TypeFor[int]())
	unrelated := reflectx.NamedTypeOf("example/roots", "Value", reflect.TypeFor[int]())
	reflect.SliceOf(unrelated)
	signature := reflect.FuncOf([]reflect.Type{dependency}, []reflect.Type{dependency}, false)
	root := reflectx.InterfaceOf(nil, []reflect.Method{{Name: "Use", Type: signature}})
	snapshot, err := Export([]reflect.Type{root, root})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.IDs[root] == 0 || snapshot.IDs[signature] == 0 || snapshot.IDs[dependency] == 0 {
		t.Fatal("root lost its method signature or type dependency")
	}
	if snapshot.IDs[unrelated] != 0 {
		t.Fatal("export included an unrelated cached type with the same name")
	}
	single, err := Export([]reflect.Type{root})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(single.Data, snapshot.Data) {
		t.Fatal("repeated roots duplicated type records")
	}
	table, err := Open(snapshot.Data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := table.Resolve(snapshot.IDs[root])
	if err != nil {
		t.Fatal(err)
	}
	dep, err := table.Resolve(snapshot.IDs[dependency])
	if err != nil {
		t.Fatal(err)
	}
	method := got.Method(0)
	if method.Name != "Use" || method.Type.In(0) != dep || method.Type.Out(0) != dep {
		t.Fatal("restored interface lost its signature dependency")
	}

	empty, err := Export(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.IDs) != 0 || len(empty.Methods) != 0 {
		t.Fatal("empty roots exported cached types or methods")
	}
	if _, err := Open(empty.Data); err != nil {
		t.Fatal(err)
	}
	if _, err := Export([]reflect.Type{nil}); err == nil || !strings.Contains(err.Error(), "nil reflectx root type") {
		t.Fatalf("nil root: %v", err)
	}
}
