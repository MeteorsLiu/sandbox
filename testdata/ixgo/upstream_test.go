//go:build linux && (amd64 || arm64) && cgo

package ixgo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goplus/ixgo"
	_ "github.com/goplus/ixgo/pkg/bytes"
	_ "github.com/goplus/ixgo/pkg/errors"
	_ "github.com/goplus/ixgo/pkg/fmt"
	_ "github.com/goplus/ixgo/pkg/go/constant"
	_ "github.com/goplus/ixgo/pkg/io"
	_ "github.com/goplus/ixgo/pkg/log"
	_ "github.com/goplus/ixgo/pkg/math"
	_ "github.com/goplus/ixgo/pkg/os"
	_ "github.com/goplus/ixgo/pkg/reflect"
	_ "github.com/goplus/ixgo/pkg/runtime"
	_ "github.com/goplus/ixgo/pkg/strings"
	_ "github.com/goplus/ixgo/pkg/sync"
	_ "github.com/goplus/ixgo/pkg/time"
	"github.com/xgo-dev/sandbox"
)

// The TestTestdataFiles corpus from ixgo v1.1.6, with static.go listed once.
func TestUpstreamPrograms(t *testing.T) {
	library := os.Getenv("SANDBOX_TEST_LIBRARY")
	if library == "" {
		t.Fatal("SANDBOX_TEST_LIBRARY must point to the matching Sentry shared library")
	}
	for _, file := range []string{
		"boundmeth.go", "complit.go", "coverage.go", "defer.go", "fieldprom.go",
		"ifaceconv.go", "ifaceprom.go", "initorder.go", "methprom.go", "mrvchain.go",
		"range.go", "recover.go", "reflect.go", "static.go", "recover2.go",
		"issue23536.go", "tinyfin.go", "issue5963.go",
	} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join("testdata", file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			interp, err := ixgo.NewContext(ixgo.SupportMultipleInterp).LoadInterp(path, data)
			if err != nil {
				t.Fatal(err)
			}
			defer interp.UnsafeRelease()
			if err := interp.RunInit(); err != nil {
				t.Fatal(err)
			}
			value, ok := interp.GetFunc("main")
			if !ok {
				t.Fatal("fixture has no main function")
			}
			fn := value.(func())
			completed := false
			s := sandbox.Sandbox{Library: library}
			if err := s.Run(func() { fn(); completed = true }); err != nil {
				t.Fatal(err)
			}
			if !completed {
				t.Fatal("guest completion was not written back")
			}
		})
	}
}
