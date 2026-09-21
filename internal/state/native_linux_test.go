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
	"unsafe"
)

func nativeDouble(n int) int { return n * 2 }

//go:noinline
func nativeCaptureFree() func(int) int { return func(n int) int { return n * 2 } }

var nativeGlobalCaptureFree = func(n int) int { return n * 2 }

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
	layout, err := m.layout(pc, false)
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
		got, err := m.layout(pc, false)
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
	layout := native.layout(reflect.ValueOf(fn).Pointer(), false)
	for i := 1; i < layout.NumField(); i++ {
		if layout.Field(i).Type == reflect.TypeFor[reflect.Value]() {
			return
		}
	}
	t.Fatal("ELF closure layout omitted the reflect.Value capture")
}

func TestNativeFunction(t *testing.T) {
	for _, src := range []func(int) int{nil, nativeDouble, nativeCaptureFree(), nativeGlobalCaptureFree, func(n int) int { return n * 2 }} {
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

func TestNativeUnreachableMethod(t *testing.T) {
	m, err := loadNativeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	sym, ok := m.names["runtime.unreachableMethod"]
	if !ok {
		t.Fatal("missing runtime.unreachableMethod symbol")
	}
	pc := uintptr(sym.Value)
	// reflectx.MethodX builds a heap funcval even for linker placeholders.
	// Its reflected signature can include a receiver, but it has no captures.
	storage := new(uintptr)
	*storage = pc
	var fn func(*nativeMethodReceiver, int) int
	*(*unsafe.Pointer)(unsafe.Pointer(&fn)) = unsafe.Pointer(storage)
	mem := make([]byte, 4096)
	var graph State
	n, _, err := graph.Save(context.Background(), mem, &fn)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.saved.objectsByID) != 1 {
		t.Fatalf("placeholder emitted %d objects, want only the function", len(graph.saved.objectsByID))
	}
	r := reader{mem: mem[:n]}
	for range 2 {
		length, objects, err := readHeader(&r)
		if err != nil || objects {
			t.Fatalf("type table header: objects=%t err=%v", objects, err)
		}
		r.readBytes(length)
	}
	count, objects, err := readHeader(&r)
	if err != nil || !objects || count != 1 {
		t.Fatalf("object header: count=%d objects=%t err=%v", count, objects, err)
	}
	id, err := r.get()
	if err != nil || id != uintValue(1) {
		t.Fatalf("root ID: %v, %v", id, err)
	}
	encoded, err := r.get()
	if err != nil {
		t.Fatal(err)
	}
	record, ok := encoded.(*functionValue)
	if !ok || record.PC != uintValue(pc) || record.Env.Root != 0 {
		t.Fatalf("placeholder record: %#v", record)
	}
	var restored func(*nativeMethodReceiver, int) int
	if _, err := Load(context.Background(), mem[:n], &restored); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	if restored == nil || reflect.ValueOf(restored).Pointer() != pc {
		t.Fatal("placeholder was dropped or its PC changed")
	}
	// Classification must not require scanning any factory instructions.
	m.path = filepath.Join(t.TempDir(), "missing-executable")
	if layout, err := m.layout(pc, false); err != nil || layout != reflect.TypeFor[struct{ F uintptr }]() {
		t.Fatalf("placeholder layout: %v, %v", layout, err)
	}
	if len(m.scanned) != 0 {
		t.Fatalf("placeholder lookup scanned %d factories", len(m.scanned))
	}
}

func TestNativeCaptureFreeLayout(t *testing.T) {
	m, err := loadNativeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.layouts) != 0 {
		t.Fatal("loaded layouts before inspecting a function value")
	}
	// Static function values must resolve without reading factory instructions.
	m.path = filepath.Join(t.TempDir(), "missing-executable")
	for _, fn := range []func(int) int{nativeCaptureFree(), nativeGlobalCaptureFree} {
		pc := reflect.ValueOf(fn).Pointer()
		addr := uintptr(*(*unsafe.Pointer)(unsafe.Pointer(&fn)))
		if addr < m.funcStart || addr >= m.funcEnd {
			t.Fatal("capture-free funcval is outside the static descriptor range")
		}
		layout, err := m.layout(pc, true)
		if err != nil {
			t.Fatal(err)
		}
		if layout != reflect.TypeFor[struct{ F uintptr }]() {
			t.Fatalf("capture-free function has environment: %v", layout)
		}
		if cached, err := m.layout(pc, false); err != nil || cached != layout {
			t.Fatalf("cached capture-free layout: %v, %v", cached, err)
		}
	}
	// An unavailable allocation is not evidence that a closure has no captures.
	fn := nativeScalarClosure(42)
	pc := reflect.ValueOf(fn).Pointer()
	if _, err := m.layout(pc, false); err == nil {
		t.Fatal("capturing closure accepted without its environment layout")
	}
	runtime.KeepAlive(fn)
}

func TestNativeCaptureFreeRecord(t *testing.T) {
	fn := nativeCaptureFree()
	pc := reflect.ValueOf(fn).Pointer()
	mem := make([]byte, 4096)
	for i := 0; i < 3; i++ {
		n, _, err := Save(context.Background(), mem, &fn)
		if err != nil {
			t.Fatal(err)
		}
		r := reader{mem: mem[:n]}
		typeBytes, isObject, err := readHeader(&r)
		if err != nil || isObject || typeBytes != 0 {
			t.Fatalf("capture-free function emitted a type table: %d, %v, %v", typeBytes, isObject, err)
		}
		typeBytes, isObject, err = readHeader(&r)
		if err != nil || isObject || typeBytes != 0 {
			t.Fatalf("capture-free function emitted a reflectx table: %d, %v, %v", typeBytes, isObject, err)
		}
		count, isObject, err := readHeader(&r)
		if err != nil || !isObject || count != 1 {
			t.Fatalf("capture-free function emitted environment objects: %d, %v, %v", count, isObject, err)
		}
		if id, err := r.get(); err != nil || id != uintValue(1) {
			t.Fatalf("root object: %v, %v", id, err)
		}
		obj, err := r.get()
		if err != nil {
			t.Fatal(err)
		}
		f, ok := obj.(*functionValue)
		if !ok || f.PC != uintValue(pc) || f.Env.Root != 0 || r.pos != n {
			t.Fatalf("capture-free record: %#v, read=%d written=%d", obj, r.pos, n)
		}
		fn = nil
		if _, err := Load(context.Background(), mem[:n], &fn); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		if got := fn(21); got != 42 {
			t.Fatalf("round trip %d returned %d", i, got)
		}
	}
}

func TestNativeCaptureFreeLayoutConflict(t *testing.T) {
	m, err := loadNativeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	fn := nativeScalarClosure(42)
	pc := reflect.ValueOf(fn).Pointer()
	layout, err := m.layout(pc, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.layout(pc, true); err == nil {
		t.Fatal("accepted a capture-free record for a known capturing closure")
	}
	if cached, err := m.layout(pc, false); err != nil || cached != layout {
		t.Fatalf("conflicting record changed cached layout: %v, %v", cached, err)
	}
	runtime.KeepAlive(fn)
}

func TestNativeCaptureFreeNewProcess(t *testing.T) {
	const imageEnv = "SANDBOX_STATE_CAPTURE_FREE_TEST_IMAGE"
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
		if got := restored(21); got != 42 {
			t.Fatalf("restored capture-free function returned %d", got)
		}
		m, err := executableNativeMetadata()
		if err != nil {
			t.Fatal(err)
		}
		addr := uintptr(*(*unsafe.Pointer)(unsafe.Pointer(&restored)))
		if addr >= m.funcStart && addr < m.funcEnd {
			t.Fatal("restored funcval unexpectedly reused a static descriptor")
		}
		var again func(int) int
		roundtrip(t, &restored, &again)
		if got := again(21); got != 42 {
			t.Fatalf("resaved capture-free function returned %d", got)
		}
		return
	}
	fn := nativeCaptureFree()
	data := make([]byte, 1<<20)
	n, _, err := Save(context.Background(), data, &fn)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "capture-free.state")
	if err := os.WriteFile(path, data[:n], 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestNativeCaptureFreeNewProcess$")
	cmd.Env = append(os.Environ(), imageEnv+"="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
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
	for name, env := range map[string]refValue{"without-env": {}, "with-env": {Root: 2}} {
		t.Run(name, func(t *testing.T) {
			w := writer{mem: make([]byte, 1024)}
			if err := writeHeader(&w, 0, false); err != nil {
				t.Fatal(err)
			}
			if err := writeHeader(&w, 0, false); err != nil {
				t.Fatal(err)
			}
			if err := writeHeader(&w, 1, true); err != nil {
				t.Fatal(err)
			}
			for _, obj := range []object{uintValue(1), &functionValue{PC: 1, Env: env}} {
				if err := w.put(obj); err != nil {
					t.Fatal(err)
				}
			}
			var dst func()
			if _, err := Load(context.Background(), w.mem[:w.pos], &dst); err == nil || !strings.Contains(err.Error(), "no supported native closure layout") {
				t.Fatalf("invalid PC: %v", err)
			}
		})
	}
}
