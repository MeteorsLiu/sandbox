//go:build linux && (amd64 || arm64)

package state

import (
	"reflect"
	"runtime"
	"testing"
)

func TestChannelCapturedByClosure(t *testing.T) {
	c := make(chan int, 2)
	c <- 41
	c <- 42
	src := func() int { return <-c }
	var dst func() int
	roundtrip(t, &src, &dst)
	runtime.GC()
	if dst() != 41 || dst() != 42 || len(c) != 2 {
		t.Fatal("closure channel capture lost its queue or shares the source")
	}
}

func TestChannelBufferedMakeFunc(t *testing.T) {
	n := 40
	fn := reflect.MakeFunc(reflect.TypeFor[func() int](), func([]reflect.Value) []reflect.Value {
		n++
		return []reflect.Value{reflect.ValueOf(n)}
	}).Interface().(func() int)
	src := make(chan func() int, 2)
	src <- fn
	src <- fn
	close(src)
	var dst chan func() int
	roundtrip(t, &src, &dst)
	runtime.GC()
	first, second := <-dst, <-dst
	if first() != 41 || second() != 42 || n != 40 || len(src) != 2 {
		t.Fatal("buffered MakeFunc lost its callback, shared capture or source isolation")
	}
}
