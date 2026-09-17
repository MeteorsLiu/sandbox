//go:build linux && (amd64 || arm64)

package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/goplus/reflectx"
)

type methodNumber interface{ Number() int }
type methodAdder interface{ Add(...int) int }

type methodRoot struct {
	Type     reflect.Type
	Object   any
	Number   methodNumber
	Adder    methodAdder
	Counter  *int
	Function reflect.Value
}

func methodFixture() methodRoot {
	typ := reflectx.NamedTypeOf("example/methodgraph", "Counter", reflect.TypeFor[struct{ N int }]())
	typ = reflectx.NewMethodSet(typ, 1, 3)
	counter := new(int)
	*counter = 3
	number := func(args []reflect.Value) []reflect.Value {
		if args[0].Type() != typ {
			panic("method receiver type changed")
		}
		return []reflect.Value{reflect.ValueOf(int(args[0].Field(0).Int()) + *counter)}
	}
	add := func(args []reflect.Value) []reflect.Value {
		value := args[0].Elem().Field(0)
		for i := 0; i < args[1].Len(); i++ {
			value.SetInt(value.Int() + args[1].Index(i).Int())
		}
		*counter++
		return []reflect.Value{reflect.ValueOf(int(value.Int()))}
	}
	hidden := func([]reflect.Value) []reflect.Value { return nil }
	if err := reflectx.SetMethodSet(typ, []reflectx.Method{
		reflectx.MakeMethod("Number", "", false, reflect.TypeFor[func() int](), number),
		reflectx.MakeMethod("Add", "", true, reflect.TypeFor[func(...int) int](), add),
		reflectx.MakeMethod("hidden", "example/private", true, reflect.TypeFor[func()](), hidden),
	}, false); err != nil {
		panic(err)
	}
	value := reflect.New(typ)
	value.Elem().Field(0).SetInt(10)
	method, _ := reflectx.MethodByName(typ, "Number")
	return methodRoot{Type: typ, Object: value.Interface(), Number: value.Interface().(methodNumber), Adder: value.Interface().(methodAdder), Counter: counter, Function: method.Func}
}

func checkMethodRoot(t *testing.T, got methodRoot) {
	t.Helper()
	value := reflect.ValueOf(got.Object)
	if value.Type().Elem() != got.Type || got.Number != got.Object || got.Adder != got.Object {
		t.Fatal("method receiver identity changed")
	}
	runtime.GC()
	if got.Number.Number() != 13 {
		t.Fatal("interface lost value receiver or capture")
	}
	if got.Function.Call([]reflect.Value{value.Elem()})[0].Int() != 13 {
		t.Fatal("function and method disagree")
	}
	if got.Adder.Add(2, 5) != 17 || *got.Counter != 4 || got.Number.Number() != 21 {
		t.Fatal("variadic method lost mutation or captured alias")
	}
	if value.Elem().Interface().(methodNumber).Number() != 21 {
		t.Fatal("value receiver interface failed")
	}
	private := reflectx.InterfaceOf(nil, []reflect.Method{{Name: "hidden", PkgPath: "example/private", Type: reflect.TypeFor[func()]()}})
	other := reflectx.NewContext().InterfaceOf(nil, []reflect.Method{{Name: "hidden", PkgPath: "example/other", Type: reflect.TypeFor[func()]()}})
	if !value.Type().Implements(private) || value.Type().Implements(other) {
		t.Fatal("private method package identity changed")
	}
	method, ok := reflectx.MethodByName(value.Type(), "hidden")
	if !ok {
		t.Fatal("private method missing")
	}
	method.Func.Call([]reflect.Value{value})
}

func TestReflectxMethodGraph(t *testing.T) {
	if os.Getenv("SANDBOX_METHOD_GRAPH_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestReflectxMethodGraph$", "-test.v")
		cmd.Env = append(os.Environ(), "SANDBOX_METHOD_GRAPH_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("method graph: %v\n%s", err, output)
		}
		return
	}
	src := methodFixture()
	var dst methodRoot
	roundtrip(t, &src, &dst)
	checkMethodRoot(t, dst)
	if *src.Counter != 3 || src.Number.Number() != 13 {
		t.Fatal("guest changed source captures")
	}
}

func TestReflectxMethodNewProcess(t *testing.T) {
	const imageEnv = "SANDBOX_METHOD_IMAGE"
	if path := os.Getenv(imageEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got methodRoot
		if _, err := Load(context.Background(), data, &got); err != nil {
			t.Fatal(err)
		}
		checkMethodRoot(t, got)
		return
	}
	if os.Getenv("SANDBOX_METHOD_IMAGE_SOURCE") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestReflectxMethodNewProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "SANDBOX_METHOD_IMAGE_SOURCE=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("method source: %v\n%s", err, output)
		}
		return
	}
	src := methodFixture()
	mem := make([]byte, 8<<20)
	n, _, err := Save(context.Background(), mem, &src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "methods.state")
	if err := os.WriteFile(path, mem[:n], 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReflectxMethodNewProcess$", "-test.v")
	cmd.Env = append(os.Environ(), imageEnv+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("method destination: %v\n%s", err, output)
	}
}
