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

func nativeDouble(n int) int { return n * 2 }

func nativeELFClosure(n *int) func() int {
	value := reflect.ValueOf(n).Elem()
	return func() int {
		value.SetInt(value.Int() + 1)
		return int(value.Int())
	}
}

func TestNativeELFLayoutCache(t *testing.T) {
	m, err := loadNativeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	n := 42
	fn := nativeELFClosure(&n)
	pc := reflect.ValueOf(fn).Pointer()
	layout, err := m.layout(pc)
	if err != nil {
		t.Fatal(err)
	}
	if layout.Size() != 32 || layout.NumField() != 2 || layout.Field(1).Type != reflect.TypeFor[reflect.Value]() || layout.Field(1).Offset != 8 {
		t.Fatalf("unexpected environment: %v", layout)
	}
	if len(m.scanned) != 1 {
		t.Fatalf("scanned %d functions, want only the factory", len(m.scanned))
	}
	for name := range m.scanned {
		t.Logf("read %s: %d bytes", name, m.names[name].Size)
	}
	// A cached lookup must not reopen or read the executable.
	m.path = filepath.Join(t.TempDir(), "missing-executable")
	for i := 0; i < 3; i++ {
		got, err := m.layout(pc)
		if err != nil || got != layout {
			t.Fatalf("cached lookup: %v, %v", got, err)
		}
	}
	runtime.KeepAlive(fn)
}

func nativeInlineClosure(n *int) func() int {
	return func() int { *n++; return *n }
}

func TestNativeInlinedFactory(t *testing.T) {
	n := 10
	fn := nativeInlineClosure(&n)
	var dst func() int
	roundtrip(t, &fn, &dst)
	if got := dst(); got != 11 || n != 10 {
		t.Fatalf("inlined factory: result=%d, source=%d", got, n)
	}
}

func TestNativeFactoryWithMethodValue(t *testing.T) {
	method := reflectedNamed{Value: 42}.Number
	t.Logf("unrelated method value: %p", method)
	n := 10
	fn := func() int { n++; return n }
	var dst func() int
	roundtrip(t, &fn, &dst)
	if got := dst(); got != 11 || n != 10 {
		t.Fatalf("factory with method value: result=%d, source=%d", got, n)
	}
}

//go:noinline
func nativeScalarClosure(n int) func() int { return func() int { return n } }

func TestNativeScalarCapture(t *testing.T) {
	fn := nativeScalarClosure(42)
	var dst func() int
	roundtrip(t, &fn, &dst)
	if got := dst(); got != 42 {
		t.Fatalf("scalar capture: %d", got)
	}
}

func requireNativeValueCapture(t *testing.T, fn any) {
	t.Helper()
	var native nativeState
	layout := native.layout(reflect.ValueOf(fn).Pointer())
	for i := 1; i < layout.NumField(); i++ {
		if layout.Field(i).Type == reflect.TypeFor[reflect.Value]() {
			return
		}
	}
	t.Fatal("ELF closure layout omitted the reflect.Value capture")
}

func TestNativeFunction(t *testing.T) {
	for _, src := range []func(int) int{nil, nativeDouble} {
		var dst func(int) int
		roundtrip(t, &src, &dst)
		if src == nil {
			if dst != nil {
				t.Fatal("nil function became non-nil")
			}
		} else if dst(21) != 42 {
			t.Fatal("restored static function returned a different result")
		}
	}
}

func TestNativeCaptures(t *testing.T) {
	type counter struct {
		n int
	}
	c := &counter{n: 10}
	prefix := "value"
	values := []int{1, 2, 3}
	var payload any = c
	fn := func(delta int) (string, int) {
		c.n += delta
		values[0]++
		return prefix, payload.(*counter).n + values[0]
	}
	var dst func(int) (string, int)
	roundtrip(t, &fn, &dst)
	runtime.GC()
	if s, n := dst(5); s != "value" || n != 17 {
		t.Fatalf("restored captures: %q, %d", s, n)
	}
	if c.n != 10 || values[0] != 1 {
		t.Fatal("restored function mutated source captures")
	}
}

func TestNativeSharedEnvironment(t *testing.T) {
	n := 10
	inc := func() int { n++; return n }
	read := func() int { return n }
	src := []func() int{inc, inc, read, nil}
	var dst []func() int
	roundtrip(t, &src, &dst)
	if dst[0]() != 11 || dst[1]() != 12 || dst[2]() != 12 || dst[3] != nil || n != 10 {
		t.Fatal("function alias or shared capture was lost")
	}
}

func TestNativeInterfaceTypes(t *testing.T) {
	type numbers []int
	type entries map[string]int
	type callback func(int) int
	items := numbers{10}
	values := []any{items, entries{"n": 20}, callback(nativeDouble)}
	fn := func() int {
		s := values[0].(numbers)
		s[0]++
		return values[2].(callback)(s[0]) + values[1].(entries)["n"]
	}
	var dst func() int
	roundtrip(t, &fn, &dst)
	if got := dst(); got != 42 || items[0] != 10 {
		t.Fatalf("named capture types or isolation lost: result=%d, source=%v", got, items)
	}
}

func TestNativeRecursiveClosure(t *testing.T) {
	var factorial func(int) int
	factorial = func(n int) int {
		if n < 2 {
			return 1
		}
		return n * factorial(n-1)
	}
	var dst func(int) int
	roundtrip(t, &factorial, &dst)
	factorial = nil
	runtime.GC()
	if got := dst(5); got != 120 {
		t.Fatalf("recursive closure returned %d", got)
	}
}

func TestNativeCapturedGraph(t *testing.T) {
	type node struct {
		value int
		next  *node
	}
	p := &node{value: 7}
	p.next = p
	q := &p.value
	src := func() bool { *q++; return p.next == p && *q == p.value }
	var dst func() bool
	roundtrip(t, &src, &dst)
	if !dst() || p.value != 7 {
		t.Fatal("capture cycle, interior pointer or isolation was lost")
	}
}

func TestNativeNestedClosure(t *testing.T) {
	n := 10
	inner := func(x int) int { n += x; return n }
	outer := func(x int) int { return inner(x) }
	var dst func(int) int
	roundtrip(t, &outer, &dst)
	if dst(5) != 15 || dst(2) != 17 || n != 10 {
		t.Fatal("nested closure environment was lost")
	}
}

func TestNativeConcurrent(t *testing.T) {
	for i := 0; i < 4; i++ {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			t.Parallel()
			n := 10
			src := func() int { n++; return n }
			var dst func() int
			roundtrip(t, &src, &dst)
			if dst() != 11 || n != 10 {
				t.Fatal("concurrent transfer changed captures")
			}
		})
	}
}

func TestNativeNewProcess(t *testing.T) {
	const imageEnv = "SANDBOX_STATE_NATIVE_TEST_IMAGE"
	if path := os.Getenv(imageEnv); path != "" {
		mem, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fn func() int
		if _, err := Load(context.Background(), mem, &fn); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		if got := fn(); got != 42 {
			t.Fatalf("restored closure returned %d", got)
		}
		return
	}
	type captured struct {
		n int
	}
	p := &captured{n: 40}
	var value any = p
	typ := reflect.StructOf([]reflect.StructField{{Name: "Result", Type: reflect.TypeFor[int](), Tag: `state:"closure"`}})
	field := reflect.ValueOf(&p.n).Elem()
	fn := func() int {
		field.SetInt(field.Int() + 1)
		result := reflect.New(typ).Elem()
		result.Field(0).SetInt(int64(value.(*captured).n + 1))
		return int(result.Field(0).Int())
	}
	requireNativeValueCapture(t, fn)
	mem := make([]byte, 1<<20)
	n, _, err := Save(context.Background(), mem, &fn)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "closure.state")
	if err := os.WriteFile(path, mem[:n], 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestNativeNewProcess$")
	cmd.Env = append(os.Environ(), imageEnv+"="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if p.n != 40 {
		t.Fatal("child changed the original capture")
	}
}

func TestNativeReflectValue(t *testing.T) {
	n := 42
	value := reflect.ValueOf(&n).Elem()
	fn := func() int {
		value.SetInt(value.Int() + 1)
		return int(value.Int())
	}
	requireNativeValueCapture(t, fn)
	var dst func() int
	roundtrip(t, &fn, &dst)
	if got := dst(); got != 43 || n != 42 {
		t.Fatalf("reflected capture: result=%d, source=%d", got, n)
	}
}

func TestNativeInvalidPC(t *testing.T) {
	w := writer{mem: make([]byte, 1024)}
	if err := writeHeader(&w, 0, false); err != nil {
		t.Fatal(err)
	}
	if err := writeHeader(&w, 1, true); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []object{uintValue(1), &functionValue{PC: 1, Env: refValue{Root: 2}}} {
		if err := w.put(obj); err != nil {
			t.Fatal(err)
		}
	}
	var dst func()
	if _, err := Load(context.Background(), w.mem[:w.pos], &dst); err == nil || !strings.Contains(err.Error(), "no supported native closure layout") {
		t.Fatalf("invalid PC: %v", err)
	}
}
