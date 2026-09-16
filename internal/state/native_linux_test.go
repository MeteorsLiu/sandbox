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
	fn := func() int {
		p.n++
		result := reflect.New(typ).Elem()
		result.Field(0).SetInt(int64(value.(*captured).n + 1))
		return int(result.Field(0).Int())
	}
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

func TestNativeRejectReflectFunction(t *testing.T) {
	src := reflect.MakeFunc(reflect.TypeFor[func()](), func([]reflect.Value) []reflect.Value { return nil }).Interface().(func())
	if _, _, err := Save(context.Background(), make([]byte, 4096), &src); err == nil {
		t.Fatal("accepted reflect.MakeFunc without a native capture layout")
	}
}
