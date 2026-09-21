//go:build linux && (amd64 || arm64)

package state

import (
	"context"
	"reflect"
	"runtime"
	"testing"
)

func TestReflectMethodValueGraph(t *testing.T) {
	type namedFunc func(int) int
	type root struct {
		Receiver *nativeMethodReceiver
		Method   reflect.Value
		Func     func(int) int
		Named    namedFunc
		Boxed    any
		Map      map[string]func(int) int
		Slice    []func(int) int
	}
	r := &nativeMethodReceiver{N: 10}
	r.Next = r
	method := reflect.ValueOf(r).MethodByName("Add")
	fn := method.Interface().(func(int) int)
	src := root{r, method, fn, namedFunc(fn), fn, map[string]func(int) int{"fn": fn}, []func(int) int{fn}}
	var dst root
	roundtrip(t, &src, &dst)
	runtime.GC()
	if dst.Receiver == r || dst.Receiver.Next != dst.Receiver || dst.Method.CanAddr() || dst.Method.Type() != method.Type() {
		t.Fatal("receiver identity, cycle or reflected method shape changed")
	}
	storage := methodValueStorage(reflect.ValueOf(dst.Func))
	for _, value := range []any{dst.Named, dst.Boxed, dst.Map["fn"], dst.Slice[0]} {
		if methodValueStorage(reflect.ValueOf(value)) != storage {
			t.Fatal("copies or conversions of a method wrapper lost identity")
		}
	}
	if dst.Method.Call([]reflect.Value{reflect.ValueOf(1)})[0].Int() != 11 ||
		dst.Func(2) != 13 || dst.Named(3) != 16 || dst.Boxed.(func(int) int)(4) != 20 ||
		dst.Map["fn"](5) != 25 || dst.Slice[0](6) != 31 || dst.Receiver.N != 31 || r.N != 10 {
		t.Fatal("reflected functions lost shared receiver or source isolation")
	}
}

func TestReflectMethodValueReceivers(t *testing.T) {
	var nilReceiver *nativeMethodReceiver
	var iface nativeMethodInterface = &nativeMethodReceiver{N: 40}
	for _, test := range []struct {
		name     string
		receiver reflect.Value
		method   string
		args     []reflect.Value
		want     any
	}{
		{"value", reflect.ValueOf(nativeMethodReceiver{N: 42}), "Value", nil, 42},
		{"scalar", reflect.ValueOf(nativeScalarReceiver(42)), "Value", nil, 42},
		{"empty", reflect.ValueOf(nativeEmptyReceiver{}), "Value", nil, 42},
		{"nil", reflect.ValueOf(nilReceiver), "IsNil", nil, true},
		{"promoted", reflect.ValueOf(nativePromotedReceiver{&nativeMethodReceiver{N: 42}}), "Value", nil, 42},
		{"interface", reflect.ValueOf(&iface).Elem(), "Add", []reflect.Value{reflect.ValueOf(2)}, 42},
	} {
		t.Run(test.name, func(t *testing.T) {
			src := test.receiver.MethodByName(test.method)
			var dst reflect.Value
			roundtrip(t, &src, &dst)
			runtime.GC()
			if got := dst.Call(test.args)[0].Interface(); got != test.want {
				t.Fatalf("method returned %v, want %v", got, test.want)
			}
		})
	}
}

type methodValueRecursive struct{ Fn func(int) int }

func (r *methodValueRecursive) Factorial(n int) int {
	if n < 2 {
		return 1
	}
	return n * r.Fn(n-1)
}

func TestReflectMethodValueRecursive(t *testing.T) {
	src := new(methodValueRecursive)
	src.Fn = reflect.ValueOf(src).MethodByName("Factorial").Interface().(func(int) int)
	var dst *methodValueRecursive
	roundtrip(t, &src, &dst)
	src.Fn = nil
	runtime.GC()
	if got := dst.Fn(5); got != 120 {
		t.Fatalf("recursive method returned %d, want 120", got)
	}
}

type methodValueLayout struct{ N int }

func (r *methodValueLayout) Sum(a, b, c, d, e, f, g, h, i, j, k, l, m, n, o, p, q int, ptr *int, rest ...int) (int, *int) {
	runtime.GC()
	total := r.N + a + b + c + d + e + f + g + h + i + j + k + l + m + n + o + p + q
	for _, value := range rest {
		total += value
	}
	return total, ptr
}

func TestReflectMethodValueCallLayout(t *testing.T) {
	src := reflect.ValueOf(&methodValueLayout{N: 10}).MethodByName("Sum")
	var dst reflect.Value
	roundtrip(t, &src, &dst)
	args := make([]reflect.Value, 17)
	for i := range args {
		args[i] = reflect.ValueOf(i + 1)
	}
	ptr := new(int)
	args = append(args, reflect.ValueOf(ptr), reflect.ValueOf([]int{20, 30}))
	results := dst.CallSlice(args)
	if !dst.Type().IsVariadic() || results[0].Int() != 213 || results[1].Interface().(*int) != ptr {
		t.Fatalf("variadic, register or stack arguments changed: %v", results)
	}
}

func TestReflectMethodValueAfterLoad(t *testing.T) {
	src := makeFuncWaiter{Fn: reflect.ValueOf(nativeScalarReceiver(42)).MethodByName("Value").Interface().(func() int)}
	var dst makeFuncWaiter
	roundtrip(t, &src, &dst)
	if dst.result != 42 {
		t.Fatalf("AfterLoad method returned %d, want 42", dst.result)
	}
}

func TestReflectMethodValueRoundTrip(t *testing.T) {
	r := &nativeMethodReceiver{N: 10}
	fn := reflect.ValueOf(r).MethodByName("Add").Interface().(func(int) int)
	host := []any{fn, fn, reflect.ValueOf(fn), r}
	ctx := context.Background()
	saved := newEncodeState(ctx, make([]byte, 1<<20))
	input := saveObjects(t, saved, &host)
	var guest []any
	loaded := newDecodeState(ctx, input)
	loadObjects(t, loaded, &guest)
	if got := guest[0].(func(int) int)(2); got != 12 || r.N != 10 {
		t.Fatal("guest method did not retain an independent receiver")
	}
	returned := loaded.loaded().encoder(ctx, make([]byte, 1<<20))
	output := saveObjects(t, returned, &guest)
	if returned.lastID != saved.lastID {
		t.Fatal("rebuilt method environment acquired a new object ID")
	}
	loadObjects(t, saved.saved().decoder(ctx, output), &host)
	runtime.GC()
	if host[3].(*nativeMethodReceiver) != r || r.N != 12 || fn(1) != 13 ||
		host[1].(func(int) int)(2) != 15 || host[2].(reflect.Value).Call([]reflect.Value{reflect.ValueOf(3)})[0].Int() != 18 {
		t.Fatal("method writeback lost the original receiver or function aliases")
	}
}
