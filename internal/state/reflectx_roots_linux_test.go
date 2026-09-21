//go:build linux && (amd64 || arm64)

package state

import (
	"context"
	"reflect"
	"testing"

	"github.com/goplus/reflectx"
)

func TestReflectxMethodRoots(t *testing.T) {
	for _, referenced := range []bool{false, true} {
		name := "unrelated"
		if referenced {
			name = "referenced_by_method"
		}
		t.Run(name, func(t *testing.T) {
			methodContext := reflectx.NewContext()
			t.Cleanup(methodContext.Reset)
			foreignCounter := 20
			foreignType := methodContext.NewMethodSet(reflectx.NamedTypeOf("example/roots", "Peer", reflect.TypeFor[int]()), 0, 1)
			foreignMethod := reflectx.MakeMethod("Number", "", true, reflect.TypeFor[func() int](), func([]reflect.Value) []reflect.Value {
				foreignCounter++
				return []reflect.Value{reflect.ValueOf(foreignCounter)}
			})
			if err := methodContext.SetMethodSet(foreignType, []reflectx.Method{foreignMethod}, false); err != nil {
				t.Fatal(err)
			}
			// Populate the global cache in both cases. Only the second case
			// gives the root method an actual reference to this other type.
			reflect.SliceOf(foreignType)
			var other any
			if referenced {
				other = reflect.New(foreignType).Interface()
			}
			counter := 10
			typ := methodContext.NewMethodSet(reflectx.NamedTypeOf("example/roots", "Root", reflect.TypeFor[int]()), 0, 1)
			method := reflectx.MakeMethod("Number", "", true, reflect.TypeFor[func() int](), func([]reflect.Value) []reflect.Value {
				counter++
				n := counter
				if other != nil {
					n += other.(interface{ Number() int }).Number()
				}
				return []reflect.Value{reflect.ValueOf(n)}
			})
			if err := methodContext.SetMethodSet(typ, []reflectx.Method{method}, false); err != nil {
				t.Fatal(err)
			}
			type root struct {
				Type    reflect.Type
				Value   any
				Counter *int
			}
			src := root{typ, reflect.New(typ).Interface(), &counter}
			original := src.Value
			var host, guest State
			mem := make([]byte, 1<<20)
			ctx := context.Background()
			n, _, err := host.Save(ctx, mem, &src)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := host.saved.reflectxSnapshot
			if snapshot == nil || (snapshot.IDs[foreignType] != 0) != referenced {
				t.Fatal("method roots did not distinguish a reference from a cached type")
			}
			wantMethods, wantNumber, wantForeign := 1, 11, 20
			if referenced {
				wantMethods, wantNumber, wantForeign = 2, 32, 21
			}
			if len(snapshot.Methods) != wantMethods {
				t.Fatalf("method count: got %d, want %d", len(snapshot.Methods), wantMethods)
			}
			var dst root
			if _, err := guest.Load(ctx, mem[:n], &dst); err != nil {
				t.Fatal(err)
			}
			if got := dst.Value.(interface{ Number() int }).Number(); got != wantNumber {
				t.Fatalf("restored method: got %d, want %d", got, wantNumber)
			}
			if *dst.Counter != 11 || counter != 10 || foreignCounter != 20 {
				t.Fatal("method captures lost their aliases or modified host storage")
			}
			n, _, err = guest.Save(ctx, mem, &dst)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := host.Load(ctx, mem[:n], &src); err != nil {
				t.Fatal(err)
			}
			if src.Type != typ || src.Value != original || src.Counter != &counter || counter != 11 || foreignCounter != wantForeign {
				t.Fatal("return lost type identity, object identity or method capture writes")
			}
		})
	}
}
