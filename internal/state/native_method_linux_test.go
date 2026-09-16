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

func TestNativeMethodValues(t *testing.T) {
	r := &nativeMethodReceiver{N: 42}
	for name, fn := range map[string]func() int{
		"value":    r.Value,
		"private":  r.private,
		"zero":     nativeEmptyReceiver{}.Value,
		"scalar":   nativeScalarReceiver(42).Value,
		"promoted": nativePromotedReceiver{r}.Value,
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
