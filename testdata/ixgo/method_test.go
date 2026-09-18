//go:build linux && (amd64 || arm64) && cgo

package ixgo_test

import (
	"reflect"
	"runtime"
	"testing"
)

type methodCounter struct{ N int }

func (c *methodCounter) Add(n int) int { c.N += n; return c.N }

type methodAdder interface{ Add(int) int }

func TestReflectMethodSandbox(t *testing.T) {
	counter := &methodCounter{N: 10}
	var boxed any = counter
	value := reflect.ValueOf(&boxed).Elem()
	method := reflect.ValueOf(counter).MethodByName("Add")
	fn := method.Interface().(func(int) int)
	var returned func(int) int
	var result int
	if err := runSandbox(t, func() {
		runtime.GC()
		if value.Elem().Interface().(*methodCounter) != counter {
			panic("interface lost receiver identity")
		}
		if method.Call([]reflect.Value{reflect.ValueOf(2)})[0].Int() != 12 {
			panic("bound reflected method")
		}
		m, ok := reflect.TypeOf(counter).MethodByName("Add")
		if !ok || m.Func.Call([]reflect.Value{reflect.ValueOf(counter), reflect.ValueOf(3)})[0].Int() != 15 {
			panic("reflected method expression")
		}
		if value.Elem().Interface().(methodAdder).Add(4) != 19 {
			panic("interface method")
		}
		result = fn(1)
		returned = reflect.ValueOf(counter).MethodByName("Add").Interface().(func(int) int)
	}); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	if result != 20 || counter.N != 20 || boxed.(*methodCounter) != counter {
		t.Fatal("method receiver mutation was not written back")
	}
	if got := method.Call([]reflect.Value{reflect.ValueOf(1)})[0].Int(); got != 21 || fn(1) != 22 || returned(1) != 23 || counter.N != 23 {
		t.Fatal("original or guest-created method lost its receiver")
	}
}
