//go:build linux && (amd64 || arm64)

package state

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/goplus/reflectx"
)

func TestReflectxSharedCallbackRoots(t *testing.T) {
	for _, mode := range []string{"value", "pointer"} {
		t.Run(mode, func(t *testing.T) {
			// FuncId entries are process-global; keep the two fixtures independent.
			if os.Getenv("SANDBOX_CALLBACK_ROOTS") != mode {
				command := exec.Command(os.Args[0], "-test.run=^TestReflectxSharedCallbackRoots$/^"+mode+"$", "-test.v")
				command.Env = append(os.Environ(), "SANDBOX_CALLBACK_ROOTS="+mode)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("callback roots: %v\n%s", err, output)
				}
				return
			}
			ctx := reflectx.NewContext()
			t.Cleanup(ctx.Reset)
			pointer := mode == "pointer"
			shared := 7
			read := func(args []reflect.Value) []reflect.Value {
				value := args[0]
				if pointer {
					value = value.Elem()
				}
				shared++
				return []reflect.Value{reflect.ValueOf(int(value.Field(0).Int()) + shared)}
			}
			var types [3]reflect.Type
			var values [3]any
			var counters [3]*int
			for i, name := range []string{"Historical", "Current", "Peer"} {
				valueMethods := 3
				if pointer {
					valueMethods = 0
				}
				typ := ctx.NewMethodSet(reflectx.NamedTypeOf("example/callbackroots", name, reflect.TypeFor[struct{ N int }]()), valueMethods, 3)
				counter := new(int)
				*counter = 100 * (i + 1)
				methods := []reflectx.Method{
					reflectx.MakeMethod("Read", "", pointer, reflect.TypeFor[func() int](), read),
					reflectx.MakeMethod("Unused", "", pointer, reflect.TypeFor[func()](), nil),
					reflectx.MakeMethod("Own", "", pointer, reflect.TypeFor[func() int](), func([]reflect.Value) []reflect.Value {
						*counter++
						return []reflect.Value{reflect.ValueOf(*counter)}
					}),
				}
				methods[0].FuncId, methods[1].FuncId = 1, 2
				if err := ctx.SetMethodSet(typ, methods, false); err != nil {
					t.Fatal(err)
				}
				value := reflect.New(typ)
				value.Elem().Field(0).SetInt(int64(10 * (i + 1)))
				if !pointer {
					value = value.Elem()
				}
				types[i], values[i], counters[i] = typ, value.Interface(), counter
			}
			first, _ := reflectx.MethodByName(reflect.TypeOf(values[0]), "Read")
			second, _ := reflectx.MethodByName(reflect.TypeOf(values[1]), "Read")
			if makeFuncStorage(first.Func) != makeFuncStorage(second.Func) || nativeReflectType(makeFuncStorage(second.Func).ftyp).In(0) != first.Type.In(0) {
				t.Fatal("fixture must reuse a wrapper with the historical receiver signature")
			}
			type root struct {
				Values   [2]any
				Counters [2]*int
				Shared   *int
			}
			src := root{[2]any{values[1], values[2]}, [2]*int{counters[1], counters[2]}, &shared}
			mem := make([]byte, 1<<20)
			for round := range 3 {
				var host, guest State
				n, _, err := host.Save(context.Background(), mem, &src)
				if err != nil {
					t.Fatal(err)
				}
				snapshot := host.saved.reflectxSnapshot
				if snapshot.IDs[types[0]] != 0 || snapshot.IDs[types[1]] == 0 || snapshot.IDs[types[2]] == 0 {
					t.Fatal("shared method imported its historical receiver type")
				}
				if len(snapshot.Methods) != 4 {
					t.Fatalf("method implementations: got %d, want 4", len(snapshot.Methods))
				}
				for _, object := range host.saved.pending {
					if object.obj.Type() == reflect.TypeFor[int]() && object.obj.Addr().Interface().(*int) == counters[0] {
						t.Fatal("historical receiver's Own capture entered the graph")
					}
				}
				_, before, _ := reflectx.IcallStat()
				cached := reflectx.IcallCached()
				var dst root
				if _, err := guest.Load(context.Background(), mem[:n], &dst); err != nil {
					t.Fatal(err)
				}
				_, after, _ := reflectx.IcallStat()
				if after-before != 4 || reflectx.IcallCached() != cached {
					t.Fatalf("method import allocated %d slots, want 4 without global cache additions", after-before)
				}
				wantShared := shared
				for i, value := range dst.Values {
					wantShared++
					if got := value.(interface{ Read() int }).Read(); got != 10*(i+2)+wantShared {
						t.Fatalf("interface Read: got %d", got)
					}
					method, _ := reflectx.MethodByName(reflect.TypeOf(value), "Read")
					wantShared++
					if got := method.Func.Call([]reflect.Value{reflect.ValueOf(value)})[0].Int(); got != int64(10*(i+2)+wantShared) {
						t.Fatalf("reflection Read: got %d", got)
					}
					if got := value.(interface{ Own() int }).Own(); got != 100*(i+2)+round+1 || *dst.Counters[i] != got {
						t.Fatalf("Own lost its capture alias: got %d", got)
					}
					unused, _ := reflectx.MethodByName(reflect.TypeOf(value), "Unused")
					if !makeFuncCallback(unused.Func).IsNil() {
						t.Fatal("nil callback acquired a wrapper")
					}
					if *counters[i+1] != 100*(i+2)+round {
						t.Fatal("guest changed host captures before writeback")
					}
				}
				if *dst.Shared != wantShared || shared != wantShared-4 {
					t.Fatal("shared callbacks lost their captured alias or host isolation")
				}
				n, _, err = guest.Save(context.Background(), mem, &dst)
				if err != nil {
					t.Fatal(err)
				}
				if len(guest.saved.reflectxSnapshot.Methods) != 4 {
					t.Fatal("return added method implementations")
				}
				if _, err := host.Load(context.Background(), mem[:n], &src); err != nil {
					t.Fatal(err)
				}
				if src.Shared != &shared || shared != wantShared || *counters[0] != 100 {
					t.Fatal("return changed shared capture identity or historical receiver state")
				}
				for i := range src.Values {
					if src.Values[i] != values[i+1] || src.Counters[i] != counters[i+1] || *counters[i+1] != 100*(i+2)+round+1 {
						t.Fatal("return lost receiver identity or capture writeback")
					}
				}
			}
		})
	}
}
