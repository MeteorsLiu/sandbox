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

	"github.com/visualfc/xtype"
)

type makeFuncPoint struct{ X, Y int }
type makeFuncOtherPoint struct{ X, Y int }

type makeFuncSignatureValues struct {
	Outer     func(*makeFuncOtherPoint, int) *makeFuncOtherPoint
	Inner     func(*makeFuncPoint, int) *makeFuncPoint
	Reflected reflect.Value
	Point     *makeFuncPoint
}

func newMakeFuncSignatureValues() makeFuncSignatureValues {
	point := &makeFuncPoint{100, 200}
	inner := reflect.MakeFunc(reflect.TypeFor[func(*makeFuncPoint, int) *makeFuncPoint](), func(args []reflect.Value) []reflect.Value {
		p := args[0].Interface().(*makeFuncPoint)
		if p != point {
			panic("MakeFunc argument lost its captured pointer alias")
		}
		p.X += int(args[1].Int())
		return []reflect.Value{reflect.ValueOf(p)}
	})
	outer := xtype.ConvertFuncValue(xtype.TypeOfType(reflect.TypeFor[func(*makeFuncOtherPoint, int) *makeFuncOtherPoint]()), inner)
	return makeFuncSignatureValues{
		Outer:     outer.Interface().(func(*makeFuncOtherPoint, int) *makeFuncOtherPoint),
		Inner:     inner.Interface().(func(*makeFuncPoint, int) *makeFuncPoint),
		Reflected: outer,
		Point:     point,
	}
}

func TestMakeFuncReinterpretedSignature(t *testing.T) {
	for _, first := range []string{"outer", "inner"} {
		t.Run(first, func(t *testing.T) {
			values := newMakeFuncSignatureValues()
			if values.Outer((*makeFuncOtherPoint)(values.Point), 0) != (*makeFuncOtherPoint)(values.Point) {
				t.Fatal("source function lost its result alias")
			}
			src := []any{values.Outer, values.Inner, values.Reflected, values.Point}
			if first == "inner" {
				src[0], src[1] = src[1], src[0]
			}
			var dst []any
			roundtrip(t, &src, &dst)
			if first == "inner" {
				dst[0], dst[1] = dst[1], dst[0]
			}
			runtime.GC()
			outer := dst[0].(func(*makeFuncOtherPoint, int) *makeFuncOtherPoint)
			inner := dst[1].(func(*makeFuncPoint, int) *makeFuncPoint)
			reflected := dst[2].(reflect.Value)
			point := dst[3].(*makeFuncPoint)
			if outer((*makeFuncOtherPoint)(point), 2) != (*makeFuncOtherPoint)(point) || inner(point, 3) != point {
				t.Fatal("restored function lost its result alias")
			}
			result := reflected.Call([]reflect.Value{reflect.ValueOf((*makeFuncOtherPoint)(point)), reflect.ValueOf(4)})
			if result[0].Interface().(*makeFuncOtherPoint) != (*makeFuncOtherPoint)(point) || point.X != 109 || values.Point.X != 100 {
				t.Fatal("restored signatures lost their captures or source isolation")
			}
			callback := makeFuncCallback(reflect.ValueOf(inner)).Addr().Pointer()
			for _, fn := range []reflect.Value{reflect.ValueOf(outer), reflected} {
				if makeFuncCallback(fn).Addr().Pointer() != callback {
					t.Fatal("different signature views no longer share one MakeFunc")
				}
			}
		})
	}
}

func TestMakeFuncReinterpretedProcess(t *testing.T) {
	const imageEnv = "SANDBOX_STATE_MAKEFUNC_SIGNATURE_IMAGE"
	ctx := context.Background()
	if path := os.Getenv(imageEnv); path != "" {
		input, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var graph State
		var guest makeFuncSignatureValues
		if _, err := graph.Load(ctx, input, &guest); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		if guest.Outer((*makeFuncOtherPoint)(guest.Point), 5).X != 105 || guest.Inner(guest.Point, 7).X != 112 {
			t.Fatal("child function lost its internal signature or capture")
		}
		loaded := graph.loaded
		mem := make([]byte, 1<<20)
		n, _, err := graph.Save(ctx, mem, &guest)
		if err != nil {
			t.Fatal(err)
		}
		if len(graph.saved.objectsByID) != len(loaded.objectsByID) {
			t.Fatal("MakeFunc signature views acquired new object IDs")
		}
		if err := os.WriteFile(path, mem[:n], 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	var graph State
	host := newMakeFuncSignatureValues()
	point, original := host.Point, host.Outer
	mem := make([]byte, 1<<20)
	n, _, err := graph.Save(ctx, mem, &host)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "makefunc.state")
	if err := os.WriteFile(path, mem[:n], 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestMakeFuncReinterpretedProcess$")
	command.Env = append(os.Environ(), imageEnv+"="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}
	if point.X != 100 {
		t.Fatal("child changed host memory before writeback")
	}
	returned, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Load(ctx, returned, &host); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	if host.Point != point || point.X != 112 || original((*makeFuncOtherPoint)(point), 1).X != 113 || host.Inner(point, 2).X != 115 {
		t.Fatal("writeback lost host pointer identity or the original function capture")
	}
	if makeFuncCallback(reflect.ValueOf(host.Outer)).Addr().Pointer() != makeFuncCallback(reflect.ValueOf(host.Inner)).Addr().Pointer() {
		t.Fatal("writeback split the function's signature views")
	}
}

func TestMakeFuncCaptures(t *testing.T) {
	n := 40
	callback := func(args []reflect.Value) []reflect.Value {
		n += int(args[0].Int())
		return []reflect.Value{reflect.ValueOf(n)}
	}
	src := reflect.MakeFunc(reflect.TypeFor[func(int) int](), callback).Interface().(func(int) int)
	var dst func(int) int
	roundtrip(t, &src, &dst)
	runtime.GC()
	if got := dst(2); got != 42 || n != 40 {
		t.Fatalf("result=%d source=%d", got, n)
	}
	var again func(int) int
	roundtrip(t, &dst, &again)
	if again(3) != 45 || dst(1) != 43 {
		t.Fatal("second roundtrip lost or shared the capture")
	}
}

func TestMakeFuncSharedEnvironment(t *testing.T) {
	type counter func() int
	n := 40
	callback := func([]reflect.Value) []reflect.Value {
		n++
		return []reflect.Value{reflect.ValueOf(n)}
	}
	fn := reflect.MakeFunc(reflect.TypeFor[func() int](), callback).Interface().(func() int)
	other := reflect.MakeFunc(reflect.TypeFor[func() int](), callback).Interface().(func() int)
	src := []any{fn, fn, counter(fn), reflect.ValueOf(fn), other, &n, callback}
	var dst []any
	roundtrip(t, &src, &dst)
	first := makeFuncCallback(reflect.ValueOf(dst[0])).Addr().Pointer()
	for _, value := range []reflect.Value{reflect.ValueOf(dst[1]), reflect.ValueOf(dst[2]), dst[3].(reflect.Value)} {
		if makeFuncCallback(value).Addr().Pointer() != first {
			t.Fatal("copies or conversions of a MakeFunc wrapper lost identity")
		}
	}
	if makeFuncCallback(reflect.ValueOf(dst[4])).Addr().Pointer() == first {
		t.Fatal("distinct MakeFunc wrappers were merged")
	}
	if dst[0].(func() int)() != 41 || dst[4].(func() int)() != 42 || *dst[5].(*int) != 42 || n != 40 {
		t.Fatal("shared callback environment or source isolation was lost")
	}
	if got := dst[6].(func([]reflect.Value) []reflect.Value)(nil)[0].Int(); got != 43 {
		t.Fatalf("direct callback no longer shares its capture: %d", got)
	}
}

func TestMakeFuncNested(t *testing.T) {
	n := 40
	inner := reflect.MakeFunc(reflect.TypeFor[func([]reflect.Value) []reflect.Value](), func([]reflect.Value) []reflect.Value {
		n++
		return []reflect.Value{reflect.ValueOf([]reflect.Value{reflect.ValueOf(n)})}
	}).Interface().(func([]reflect.Value) []reflect.Value)
	src := reflect.MakeFunc(reflect.TypeFor[func() int](), inner).Interface().(func() int)
	var dst func() int
	roundtrip(t, &src, &dst)
	runtime.GC()
	if dst() != 41 || dst() != 42 || n != 40 {
		t.Fatal("nested MakeFunc wrappers lost their callback or capture")
	}
}

func TestMakeFuncRecursive(t *testing.T) {
	var src func(int) int
	src = reflect.MakeFunc(reflect.TypeFor[func(int) int](), func(args []reflect.Value) []reflect.Value {
		n := int(args[0].Int())
		if n < 2 {
			return []reflect.Value{reflect.ValueOf(1)}
		}
		return []reflect.Value{reflect.ValueOf(n * src(n-1))}
	}).Interface().(func(int) int)
	var dst func(int) int
	roundtrip(t, &src, &dst)
	src = nil
	runtime.GC()
	if got := dst(5); got != 120 {
		t.Fatalf("recursive MakeFunc: %d", got)
	}
}

func makeFuncCallLayout(args []reflect.Value) []reflect.Value {
	runtime.GC()
	total := 0
	for _, arg := range args[:17] {
		total += int(arg.Int())
	}
	for _, n := range args[18].Interface().([]int) {
		total += n
	}
	return []reflect.Value{reflect.ValueOf(total), args[17]}
}

func TestMakeFuncCallLayout(t *testing.T) {
	// Seventeen integer arguments spill to the stack on both supported ABIs.
	inputs := make([]reflect.Type, 17)
	args := make([]reflect.Value, 17)
	for i := range inputs {
		inputs[i] = reflect.TypeFor[int]()
		args[i] = reflect.ValueOf(i + 1)
	}
	inputs = append(inputs, reflect.TypeFor[*int](), reflect.TypeFor[[]int]())
	p := new(int)
	args = append(args, reflect.ValueOf(p), reflect.ValueOf([]int{20, 30}))
	typ := reflect.FuncOf(inputs, []reflect.Type{reflect.TypeFor[int](), reflect.TypeFor[*int]()}, true)
	src := reflect.MakeFunc(typ, makeFuncCallLayout)
	var dst reflect.Value
	roundtrip(t, &src, &dst)
	results := dst.CallSlice(args)
	if results[0].Int() != 203 || results[1].Interface().(*int) != p {
		t.Fatalf("arguments or results changed: %v", results)
	}
}

func TestMakeFuncCaptureFreeCallback(t *testing.T) {
	src := reflect.MakeFunc(reflect.TypeFor[func(int) int](), func(args []reflect.Value) []reflect.Value {
		return []reflect.Value{reflect.ValueOf(int(args[0].Int()) * 2)}
	}).Interface().(func(int) int)
	var dst func(int) int
	roundtrip(t, &src, &dst)
	runtime.GC()
	if got := dst(21); got != 42 {
		t.Fatalf("MakeFunc with a capture-free callback returned %d", got)
	}
}

func TestMakeFuncNilCallback(t *testing.T) {
	src := reflect.MakeFunc(reflect.TypeFor[func()](), nil).Interface().(func())
	var dst func()
	roundtrip(t, &src, &dst)
	if dst == nil || !makeFuncCallback(reflect.ValueOf(dst)).IsNil() {
		t.Fatal("MakeFunc with a nil callback changed representation")
	}
}

type makeFuncWaiter struct {
	Fn     func() int
	result int
}

func (*makeFuncWaiter) StateTypeName() string { return "state.test.makeFuncWaiter" }
func (*makeFuncWaiter) StateFields() []string { return []string{"Fn"} }
func (v *makeFuncWaiter) StateSave(s Sink)    { s.Save(0, &v.Fn) }
func (v *makeFuncWaiter) StateLoad(_ context.Context, s Source) {
	s.Load(0, &v.Fn)
	s.AfterLoad(func() { v.result = v.Fn() })
}

func init() { Register((*makeFuncWaiter)(nil)) }

func TestMakeFuncAfterLoad(t *testing.T) {
	n := 41
	src := makeFuncWaiter{Fn: reflect.MakeFunc(reflect.TypeFor[func() int](), func([]reflect.Value) []reflect.Value {
		n++
		return []reflect.Value{reflect.ValueOf(n)}
	}).Interface().(func() int)}
	var dst makeFuncWaiter
	roundtrip(t, &src, &dst)
	if dst.result != 42 || n != 41 {
		t.Fatalf("AfterLoad: result=%d source=%d", dst.result, n)
	}
}

func TestMakeFuncNewProcess(t *testing.T) {
	const imageEnv = "SANDBOX_STATE_MAKEFUNC_TEST_IMAGE"
	if path := os.Getenv(imageEnv); path != "" {
		mem, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fn reflect.Value
		if _, err := Load(context.Background(), mem, &fn); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		if got := fn.Call(nil)[0].Field(0).Int(); got != 42 {
			t.Fatalf("restored MakeFunc returned %d", got)
		}
		return
	}
	typ := reflect.StructOf([]reflect.StructField{{Name: "N", Type: reflect.TypeFor[int]()}})
	n := 41
	fn := reflect.MakeFunc(reflect.FuncOf(nil, []reflect.Type{typ}, false), func([]reflect.Value) []reflect.Value {
		n++
		value := reflect.New(typ).Elem()
		value.Field(0).SetInt(int64(n))
		return []reflect.Value{value}
	})
	mem := make([]byte, 1<<20)
	size, _, err := Save(context.Background(), mem, &fn)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "makefunc.state")
	if err := os.WriteFile(path, mem[:size], 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestMakeFuncNewProcess$")
	cmd.Env = append(os.Environ(), imageEnv+"="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if n != 41 {
		t.Fatal("child changed the original capture")
	}
}
