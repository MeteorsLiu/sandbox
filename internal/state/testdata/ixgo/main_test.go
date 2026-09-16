//go:build linux && (amd64 || arm64)

package ixgo_test

import (
	"context"
	"reflect"
	"runtime"
	"testing"

	"github.com/goplus/ixgo"
	"github.com/xgo-dev/sandbox/internal/state"
)

// Run explicitly with go test -ldflags=-checklinkname=0 ./internal/state/testdata/ixgo.
// This example expects successful migration, without an ixgo-specific codec.
const source = `package main

func Make(n int) func() int {
	return func() int {
		n++
		return n
	}
}
`

func interpretedCounter(t *testing.T) func() int {
	t.Helper()
	ctx := ixgo.NewContext(ixgo.SupportMultipleInterp)
	interp, err := ctx.LoadInterp("counter.go", source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interp.UnsafeRelease)
	if err := interp.RunInit(); err != nil {
		t.Fatal(err)
	}
	value, err := interp.RunFunc("Make", 40)
	if err != nil {
		t.Fatal(err)
	}
	return value.(func() int)
}

func checkRoundTrip(t *testing.T, fn func() int) {
	t.Helper()
	pc := reflect.ValueOf(fn).Pointer()
	t.Logf("entry: %s (%#x)", runtime.FuncForPC(pc).Name(), pc)
	if got := fn(); got != 41 {
		t.Fatalf("before Save: got %d, want 41", got)
	}

	mem := make([]byte, 1<<20)
	n, _, err := state.Save(context.Background(), mem, &fn)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Logf("saved %d bytes", n)
	var restored func() int
	if _, err := state.Load(context.Background(), mem[:n], &restored); err != nil {
		t.Fatalf("Load: %v", err)
	}
	runtime.GC()
	for _, want := range []int{42, 43} {
		if got := restored(); got != want {
			t.Fatalf("restored closure: got %d, want %d", got, want)
		}
	}
	if got := fn(); got != 42 {
		t.Fatalf("source closure: got %d, want 42; restored env must be independent", got)
	}
}

func TestNativeClosureRoundTrip(t *testing.T) {
	n := 40
	checkRoundTrip(t, func() int { n++; return n })
}

func TestReflectMakeFuncRoundTrip(t *testing.T) {
	n := 40
	fn := reflect.MakeFunc(reflect.TypeFor[func() int](), func([]reflect.Value) []reflect.Value {
		n++
		return []reflect.Value{reflect.ValueOf(n)}
	}).Interface().(func() int)
	checkRoundTrip(t, fn)
}

func TestIxgoClosureRoundTrip(t *testing.T) {
	checkRoundTrip(t, interpretedCounter(t))
}

func TestWrappedIxgoClosureRoundTrip(t *testing.T) {
	fn := interpretedCounter(t)
	checkRoundTrip(t, func() int { return fn() })
}
