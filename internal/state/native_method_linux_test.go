//go:build linux && (amd64 || arm64)

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
	methodpkg "github.com/xgo-dev/sandbox/internal/state/testdata/method.pkg"
)

type nativeMethodReceiver struct {
	N    int
	Next *nativeMethodReceiver
}

func (r *nativeMethodReceiver) Add(n int) int { r.N += n; return r.N }
func (r *nativeMethodReceiver) IsNil() bool   { return r == nil }
func (r nativeMethodReceiver) Value() int     { return r.N }
func (r nativeMethodReceiver) private() int   { return r.N }

//go:noinline
func (r *nativeMethodReceiver) Closure(n int) func() int {
	return func() int { r.N += n; return r.N }
}

type nativeMethodInterface interface{ Add(int) int }

//go:noinline
func nativeInterfaceMethod(r nativeMethodInterface) func(int) int { return r.Add }

type nativeEmptyReceiver struct{}

func (nativeEmptyReceiver) Value() int { return 42 }

type nativeScalarReceiver int

func (r nativeScalarReceiver) Value() int { return int(r) }

type nativePromotedReceiver struct{ *nativeMethodReceiver }

type nativeForeignReceiver struct{ methodpkg.Receiver }

type nativeGenericReceiver[T any] struct{ value T }

func (r *nativeGenericReceiver[T]) get() T { return r.value }

//go:noinline
func nativeGenericBind[T any](r *nativeGenericReceiver[T]) func() T { return r.get }

func TestNativePackagePrefix(t *testing.T) {
	for _, test := range []struct{ path, prefix string }{
		{"runtime", "runtime"},
		{"example.org/pkg", "example.org/pkg"},
		{"example.org/pkg.v1", "example.org/pkg%2ev1"},
		{"example.org/parent.v1/pkg", "example.org/parent.v1/pkg"},
		{"example.org/pkg%1", "example.org/pkg%251"},
		{"\x01 \"\x7f\xc3\xa9", "%01%20%22%7f%c3%a9"},
	} {
		if got := nativePackagePrefix(test.path); got != test.prefix {
			t.Errorf("PathToPrefix(%q) = %q, want %q", test.path, got, test.prefix)
		}
	}
}

func TestNativeQualifiedPrivateMethods(t *testing.T) {
	m, err := loadNativeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	m.path = filepath.Join(t.TempDir(), "missing-executable")
	value := nativeForeignReceiver{methodpkg.Receiver{N: 42}}
	if methodpkg.Value(value) != 42 || methodpkg.Value(&value) != 42 || methodpkg.Add(&value, 0) != 42 {
		t.Fatal("source private methods failed")
	}
	for _, test := range []struct {
		receiver any
		name     string
		args     []reflect.Value
	}{
		{methodpkg.Receiver{N: 42}, "value", nil},
		{&methodpkg.Receiver{N: 40}, "add", []reflect.Value{reflect.ValueOf(2)}},
		{value, "value", nil},
		{&value, "value", nil},
		{&value, "add", []reflect.Value{reflect.ValueOf(0)}},
	} {
		t.Run(reflect.TypeOf(test.receiver).String()+"."+test.name, func(t *testing.T) {
			receiver := reflect.ValueOf(test.receiver)
			method, ok := reflectx.MethodByName(receiver.Type(), test.name)
			if !ok || !method.Func.IsValid() {
				t.Fatal("private method not found")
			}
			layout, err := m.layout(method.Func.Pointer(), false)
			if err != nil || layout != reflect.TypeFor[struct{ F uintptr }]() {
				t.Fatalf("private method layout: %v, %v", layout, err)
			}
			src := method.Func
			var dst reflect.Value
			roundtrip(t, &src, &dst)
			runtime.GC()
			if got := dst.Call(append([]reflect.Value{receiver}, test.args...))[0].Int(); got != 42 {
				t.Fatalf("restored private method returned %d", got)
			}
		})
	}
	if len(m.scanned) != 0 {
		t.Fatalf("method lookup scanned %d functions", len(m.scanned))
	}
	boundValue := methodpkg.Receiver{N: 42}.BoundValue()
	var restoredValue func() int
	roundtrip(t, &boundValue, &restoredValue)
	if restoredValue() != 42 {
		t.Fatal("bound method with escaped package prefix lost its receiver")
	}
	receiver := &methodpkg.Receiver{N: 40}
	boundAdd := receiver.BoundAdd()
	var restoredAdd func(int) int
	roundtrip(t, &boundAdd, &restoredAdd)
	if restoredAdd(2) != 42 || receiver.N != 40 {
		t.Fatal("bound pointer method lost its receiver or source isolation")
	}
}

func TestNativeMethodValues(t *testing.T) {
	r := &nativeMethodReceiver{N: 42}
	for name, fn := range map[string]func() int{
		"value":    r.Value,
		"private":  r.private,
		"zero":     nativeEmptyReceiver{}.Value,
		"scalar":   nativeScalarReceiver(42).Value,
		"promoted": nativePromotedReceiver{r}.Value,
		"generic":  (&nativeGenericReceiver[int]{value: 42}).get,
	} {
		t.Run(name, func(t *testing.T) {
			var restored func() int
			roundtrip(t, &fn, &restored)
			runtime.GC()
			if got := restored(); got != 42 {
				t.Fatalf("restored method returned %d", got)
			}
		})
	}
	t.Run("generic binding", func(t *testing.T) {
		node := &nativeGenericNode{N: 42}
		node.Next = node
		src := nativeGenericBind(&nativeGenericReceiver[*nativeGenericNode]{value: node})
		var dst func() *nativeGenericNode
		roundtrip(t, &src, &dst)
		runtime.GC()
		if got := dst(); got == node || got.N != 42 || got.Next != got {
			t.Fatal("generic method binding lost the concrete receiver graph")
		}
	})
	t.Run("generic expression", func(t *testing.T) {
		src := (*nativeGenericReceiver[int]).get
		var dst func(*nativeGenericReceiver[int]) int
		roundtrip(t, &src, &dst)
		if dst(&nativeGenericReceiver[int]{value: 42}) != 42 {
			t.Fatal("generic method expression changed its result")
		}
	})
	var nilReceiver *nativeMethodReceiver
	fn := nilReceiver.IsNil
	var restored func() bool
	roundtrip(t, &fn, &restored)
	if !restored() {
		t.Fatal("nil receiver was not preserved")
	}
	// Method expressions receive the receiver as a normal argument.
	expression := (*nativeMethodReceiver).Add
	var restoredExpression func(*nativeMethodReceiver, int) int
	roundtrip(t, &expression, &restoredExpression)
	if got := restoredExpression(r, 1); got != 43 {
		t.Fatalf("method expression returned %d", got)
	}
}

func TestNativeMethodSharedReceiver(t *testing.T) {
	type root struct {
		Receiver  *nativeMethodReceiver
		First     func(int) int
		Second    func(int) int
		Interface func(int) int
	}
	r := &nativeMethodReceiver{N: 10}
	r.Next = r
	src := root{r, r.Add, r.Add, nativeInterfaceMethod(r)}
	var dst root
	roundtrip(t, &src, &dst)
	runtime.GC()
	if dst.First(1) != 11 || dst.Second(2) != 13 || dst.Interface(3) != 16 {
		t.Fatal("methods no longer share the restored receiver")
	}
	if dst.Receiver == r || dst.Receiver.N != 16 || dst.Receiver.Next != dst.Receiver || r.N != 10 {
		t.Fatal("receiver alias, cycle or isolation changed")
	}
}

func TestNativeReflectedMethods(t *testing.T) {
	m, err := loadNativeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	m.path = filepath.Join(t.TempDir(), "missing-executable")
	for _, test := range []struct {
		name string
		typ  reflect.Type
		args []reflect.Value
	}{
		{"Add", reflect.TypeFor[*nativeMethodReceiver](), []reflect.Value{reflect.ValueOf(&nativeMethodReceiver{N: 40}), reflect.ValueOf(2)}},
		{"Value", reflect.TypeFor[nativeMethodReceiver](), []reflect.Value{reflect.ValueOf(nativeMethodReceiver{N: 42})}},
		{"Value", reflect.TypeFor[*nativeMethodReceiver](), []reflect.Value{reflect.ValueOf(&nativeMethodReceiver{N: 42})}},
		{"Value", reflect.TypeFor[nativeScalarReceiver](), []reflect.Value{reflect.ValueOf(nativeScalarReceiver(42))}},
		{"Value", reflect.TypeFor[nativeEmptyReceiver](), []reflect.Value{reflect.ValueOf(nativeEmptyReceiver{})}},
	} {
		t.Run(test.typ.String()+"."+test.name, func(t *testing.T) {
			method, ok := test.typ.MethodByName(test.name)
			if !ok {
				t.Fatal("method not found")
			}
			layout, err := m.layout(method.Func.Pointer(), false)
			if err != nil {
				t.Fatal(err)
			}
			if layout != reflect.TypeFor[struct{ F uintptr }]() {
				t.Fatalf("unbound method has a captured receiver: %v", layout)
			}
			src := method.Func.Interface()
			var dst any
			roundtrip(t, &src, &dst)
			runtime.GC()
			if got := reflect.ValueOf(dst).Call(test.args)[0].Int(); got != 42 {
				t.Fatalf("restored method returned %d", got)
			}
		})
	}
	if len(m.scanned) != 0 {
		t.Fatalf("method lookup scanned %d functions", len(m.scanned))
	}
}

func TestNativeClosureInsideMethod(t *testing.T) {
	r := &nativeMethodReceiver{N: 40}
	fn := r.Closure(2)
	var restored func() int
	roundtrip(t, &fn, &restored)
	runtime.GC()
	if restored() != 42 || restored() != 44 || r.N != 40 {
		t.Fatal("closure inside a method lost its captured environment")
	}
}

func TestNativeMethodLayout(t *testing.T) {
	m, err := loadNativeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	// Resolving a method value needs neither the original method symbol nor
	// its creating function's instructions. It also works after unlinking.
	m.path = filepath.Join(t.TempDir(), "missing-executable")
	r := &nativeMethodReceiver{N: 42}
	for _, test := range []struct {
		fn   any
		recv reflect.Type
	}{
		{r.Add, reflect.TypeFor[*nativeMethodReceiver]()},
		{r.Value, reflect.TypeFor[nativeMethodReceiver]()},
		{nativeInterfaceMethod(r), reflect.TypeFor[nativeMethodInterface]()},
		{nativeEmptyReceiver{}.Value, reflect.TypeFor[nativeEmptyReceiver]()},
	} {
		pc := reflect.ValueOf(test.fn).Pointer()
		name := runtime.FuncForPC(pc).Name()
		if !strings.HasSuffix(name, "-fm") {
			t.Fatalf("expected a method value, got %s", name)
		}
		delete(m.names, strings.TrimSuffix(name, "-fm"))
		layout, err := m.layout(pc, false)
		if err != nil {
			t.Fatal(err)
		}
		if layout.NumField() != 2 || layout.Field(0).Name != "F" || layout.Field(1).Name != "R" || layout.Field(1).Type != test.recv || layout.Field(1).Offset != 8 {
			t.Fatalf("%s: unexpected receiver layout %v", name, layout)
		}
		if cached, err := m.layout(pc, false); err != nil || cached != layout {
			t.Fatalf("cached method layout: %v, %v", cached, err)
		}
	}
	if len(m.scanned) != 0 {
		t.Fatalf("method lookup scanned %d functions", len(m.scanned))
	}
}

func TestNativeMethodNewProcess(t *testing.T) {
	const imageEnv = "SANDBOX_STATE_METHOD_TEST_IMAGE"
	if path := os.Getenv(imageEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var restored func(int) int
		if _, err := Load(context.Background(), data, &restored); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		if restored(1) != 41 || restored(1) != 42 {
			t.Fatal("restored method lost its receiver in the child process")
		}
		return
	}
	r := &nativeMethodReceiver{N: 40}
	fn := r.Add
	data := make([]byte, 1<<20)
	n, _, err := Save(context.Background(), data, &fn)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "method.state")
	if err := os.WriteFile(path, data[:n], 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestNativeMethodNewProcess$")
	cmd.Env = append(os.Environ(), imageEnv+"="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if r.N != 40 {
		t.Fatal("child changed the source receiver")
	}
}
