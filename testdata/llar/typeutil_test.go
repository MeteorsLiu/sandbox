//go:build linux && (amd64 || arm64) && cgo

package formula_test

import (
	"go/token"
	"go/types"
	"os"
	"testing"

	gogentypes "github.com/goplus/gogen/typeutil"
	"github.com/xgo-dev/sandbox"
	"golang.org/x/tools/go/types/typeutil"
)

func TestTypeutilMapSandbox(t *testing.T) {
	library := os.Getenv("SANDBOX_TEST_LIBRARY")
	if library == "" {
		t.Fatal("SANDBOX_TEST_LIBRARY must point to the matching Sentry shared library")
	}
	type typeMap interface {
		At(types.Type) any
		Set(types.Type, any) any
		Len() int
	}
	for _, test := range []struct {
		name string
		m    typeMap
	}{
		{"x/tools", new(typeutil.Map)},
		{"gogen", new(gogentypes.Map)},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := test.m
			key := types.NewNamed(types.NewTypeName(token.NoPos, nil, "Node", nil), types.Typ[types.Int], nil)
			m.Set(key, 42)
			m.Set(types.NewPointer(key), m)
			completed := false
			s := sandbox.Sandbox{Library: library}
			if err := s.Run(func() {
				if m.At(key) != 42 || m.At(types.NewPointer(key)) != m || m.Len() != 2 {
					panic("type map lost its entries or self-reference")
				}
				m.Set(key, 43)
				m.Set(types.NewSlice(key), "guest")
				completed = true
			}); err != nil {
				t.Fatal(err)
			}
			if !completed || m != test.m || m.At(key) != 43 || m.At(types.NewSlice(key)) != "guest" || m.At(types.NewPointer(key)) != m || m.Len() != 3 {
				t.Fatal("type map writeback lost its entries or identity")
			}
		})
	}
}
