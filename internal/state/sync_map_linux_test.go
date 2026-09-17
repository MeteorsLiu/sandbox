//go:build linux && (amd64 || arm64)

package state

import (
	"reflect"
	"sync"
	"testing"
)

func TestSyncMapClosure(t *testing.T) {
	type root struct {
		Map sync.Map
		N   int
		Fn  func() int
	}
	src := &root{N: 40}
	src.Fn = nativeELFClosure(&src.N)
	src.Map.Store("fn", src.Fn)
	src.Map.Store("reflected", reflect.ValueOf(src.Fn))
	dst := new(root)
	roundtrip(t, src, dst)
	fn, ok := dst.Map.Load("fn")
	if !ok || fn.(func() int)() != 41 || dst.Fn() != 42 {
		t.Fatal("map closure lost its shared environment")
	}
	value, ok := dst.Map.Load("reflected")
	if !ok || value.(reflect.Value).Call(nil)[0].Int() != 43 || dst.N != 43 || src.N != 40 {
		t.Fatal("reflected map closure lost environment aliasing or isolation")
	}
}
