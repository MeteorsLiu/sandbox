//go:build linux && (amd64 || arm64) && cgo

package ixgo_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/goplus/ixgo"
	_ "github.com/goplus/ixgo/pkg/io/fs"
	_ "github.com/goplus/ixgo/pkg/runtime"
)

func registeredNativeRate() int      { return runtime.MemProfileRate }
func registeredNativeSkipDir() error { return filepath.SkipDir }

func TestIxgoNativeVariables(t *testing.T) {
	const source = `package main
import (
    "io/fs"
    "path/filepath"
    "runtime"
)
func NativeRate() int
func NativeSkipDir() error
func Check() (int, bool, bool) {
    runtime.MemProfileRate = 12345
    same := filepath.SkipDir == NativeSkipDir()
    err := filepath.WalkDir("/", func(path string, d fs.DirEntry, err error) error {
        return filepath.SkipDir
    })
    return NativeRate(), same, err == nil
}`
	originalRate := runtime.MemProfileRate
	defer func() { runtime.MemProfileRate = originalRate }()
	ctx := ixgo.NewContext(ixgo.SupportMultipleInterp)
	ctx.RegisterExternal("main.NativeRate", registeredNativeRate)
	ctx.RegisterExternal("main.NativeSkipDir", registeredNativeSkipDir)
	interp, err := ctx.LoadInterp("native_variables.go", source)
	if err != nil {
		t.Fatal(err)
	}
	defer interp.UnsafeRelease()
	if err := interp.RunInit(); err != nil {
		t.Fatal(err)
	}
	value, ok := interp.GetFunc("Check")
	if !ok {
		t.Fatal("missing Check")
	}
	check := value.(func() (int, bool, bool))
	var rate int
	var sameSentinel, walked bool
	if err := runSandbox(t, func() { rate, sameSentinel, walked = check() }); err != nil {
		t.Fatal(err)
	}
	if rate != 12345 || !sameSentinel || !walked {
		t.Fatalf("guest native rate=%d sentinel=%v walk=%v", rate, sameSentinel, walked)
	}
	if runtime.MemProfileRate != originalRate {
		t.Fatalf("guest changed host profile rate: got %d, want %d", runtime.MemProfileRate, originalRate)
	}
}
