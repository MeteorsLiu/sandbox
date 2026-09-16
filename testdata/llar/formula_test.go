//go:build linux && (amd64 || arm64) && cgo

package formula_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	formulapkg "github.com/goplus/llar/formula"
	"github.com/goplus/llar/internal/formula"
	"github.com/xgo-dev/sandbox"
)

func TestFormulaSandbox(t *testing.T) {
	library := os.Getenv("SANDBOX_TEST_LIBRARY")
	if library == "" {
		t.Fatal("SANDBOX_TEST_LIBRARY must point to the matching Sentry shared library")
	}
	for _, test := range []struct {
		file, module, version string
	}{
		{"hello_llar.gox", "DaveGamble/cJSON", "v1.0.0"},
		{"cpu_features_llar.gox", "google/cpu_features", "v0.9.0"},
		{"targetsurface_llar.gox", "test/target", "v1.0.0"},
		{"env_llar.gox", "test/env", "v1.0.0"},
		{"pkgconfigusage_llar.gox", "test/pkgconfig", "v1.0.0"},
	} {
		t.Run(test.file, func(t *testing.T) {
			f, err := formula.LoadFS(os.DirFS("testdata").(fs.ReadFileFS), test.file)
			if err != nil {
				t.Fatal(err)
			}
			if f.ModPath != test.module || f.FromVer != test.version {
				t.Fatalf("metadata: %s@%s", f.ModPath, f.FromVer)
			}
			work, err := os.MkdirTemp("", "sandbox-formula-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(work) })
			if err := os.Chmod(work, 0777); err != nil {
				t.Fatal(err)
			}
			ctx := formulapkg.NewContext(&formulapkg.Project{}, "", work, "", nil)
			deps := new(formulapkg.ModuleDeps)
			build, require, filter, verify := f.OnBuild, f.OnRequire, f.Filter, f.OnTest
			completed, accepted := false, false
			envBefore := os.Getenv("FORMULA_ENV_TEST")
			s := sandbox.Sandbox{
				Library: library,
				Mounts: []sandbox.Mount{
					{Type: "bind", Source: "/", Target: "/", Options: []string{"ro"}},
					{Type: "bind", Source: work, Target: work, Options: []string{"rw"}},
					{Type: "proc", Target: "/proc"},
				},
			}
			err = s.Run(func() {
				if filter != nil {
					accepted = filter()
				}
				if require != nil {
					require(&formulapkg.Project{}, deps)
				}
				if build != nil {
					build(ctx)
				}
				if verify != nil {
					verify(ctx)
				}
				completed = true
			})
			if err != nil {
				t.Fatal(err)
			}
			if !completed || os.Getenv("FORMULA_ENV_TEST") != envBefore {
				t.Fatal("guest result missing or host environment changed")
			}
			if test.file == "targetsurface_llar.gox" {
				got := deps.Deps()
				if !accepted || len(got) != 1 || got[0].Path != "madler/zlib" || got[0].Version != "v1.3.1" {
					t.Fatalf("filter/dependency writeback: accepted=%v deps=%v", accepted, got)
				}
			}
			if test.file == "pkgconfigusage_llar.gox" {
				data, err := os.ReadFile(filepath.Join(work, "lib", "pkgconfig", "llar-formula-pkgconfig-test.pc"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), "Requires: llar-formula-pkgconfig-dependency >= 1.0.0") || !strings.Contains(ctx.Out.Metadata(), "-lllar_formula_pkgconfig_test") {
					t.Fatalf("pkg-config output or metadata missing: %s", ctx.Out.Metadata())
				}
			}
		})
	}
}
