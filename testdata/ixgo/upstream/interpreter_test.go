//go:build linux && (amd64 || arm64) && cgo

package upstream_test

import (
	"bytes"
	"context"
	_ "embed"
	"go/ast"
	"go/format"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
)

//go:embed testdata/bridge.go.txt
var bridgeSource string

//go:embed testdata/main.go.txt
var mainSource string

// TestInterpreterUpstream preserves the pinned ixgo interpreter tests and their
// assertions. Only test call sites change: loading stays on the host, execution
// goes through Sandbox.Run, and results are restored before assertions run.
// REPL, optimizer, and direct-call tests are outside this suite.
func TestInterpreterUpstream(t *testing.T) {
	if os.Getenv("SANDBOX_TEST_LIBRARY") == "" {
		t.Fatal("SANDBOX_TEST_LIBRARY must point to the matching Sentry shared library")
	}
	const path = "github.com/goplus/ixgo"
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedImports | packages.NeedDeps | packages.NeedModule,
		Tests:      true,
		BuildFlags: []string{"-mod=readonly"},
	}, path)
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(pkgs) != 0 {
		t.Fatal("load ixgo tests")
	}
	dir, err := os.MkdirTemp("", "sandbox-ixgo-upstream-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	replacements := make(map[string]string)
	var sourceDir string
	var names []string
	for _, pkg := range pkgs {
		if pkg.PkgPath != path && pkg.PkgPath != path+"_test" {
			continue
		}
		for _, file := range pkg.Syntax {
			filename := pkg.Fset.Position(file.Pos()).Filename
			if !strings.HasSuffix(filename, "_test.go") {
				continue
			}
			if _, exists := replacements[filename]; exists {
				continue
			}
			sourceDir = filepath.Dir(filename)
			base := filepath.Base(filename)
			if base == "repl_test.go" || base == "transform_test.go" || base == "direct_call_test.go" {
				file.Decls, file.Comments = nil, nil
			} else {
				if base == "interp_test.go" {
					file.Decls = slices.DeleteFunc(file.Decls, func(decl ast.Decl) bool {
						fn, ok := decl.(*ast.FuncDecl)
						return ok && fn.Name.Name == "TestGeneratedDirectCalls"
					})
					astutil.DeleteImport(pkg.Fset, file, "github.com/goplus/ixgo/testdata/direct_call/github.com/goplus/ixgo/testdata/direct_call/pkg")
				}
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
						names = append(names, fn.Name.Name)
					}
				}
				ast.Inspect(file, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					selector, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					fn, ok := pkg.TypesInfo.Uses[selector.Sel].(*types.Func)
					if !ok || fn.Pkg() == nil || fn.Pkg().Path() != path {
						return true
					}
					sig := fn.Type().(*types.Signature)
					name := fn.Name()
					var args []ast.Expr
					if sig.Recv() == nil {
						if name != "Run" && name != "RunFile" {
							return true
						}
						args = append(args, &ast.CallExpr{
							Fun:  &ast.SelectorExpr{X: selector.X, Sel: ast.NewIdent("NewContext")},
							Args: []ast.Expr{call.Args[len(call.Args)-1]},
						})
						args = append(args, call.Args[:len(call.Args)-1]...)
					} else {
						switch name {
						case "Run", "RunFile", "RunPkg", "RunInterp", "RunTest", "RunMain", "RunInit", "RunFunc":
						default:
							return true
						}
						args = append([]ast.Expr{selector.X}, call.Args...)
						if name == "RunFunc" {
							recv := sig.Recv().Type().(*types.Pointer).Elem().(*types.Named)
							name = recv.Obj().Name() + name
						}
					}
					helper := ast.NewIdent("SandboxTest" + name)
					if pkg.PkgPath == path {
						call.Fun = helper
					} else {
						call.Fun = &ast.SelectorExpr{X: ast.NewIdent("ixgo"), Sel: helper}
					}
					call.Args = args
					return true
				})
			}
			var data bytes.Buffer
			if err := format.Node(&data, pkg.Fset, file); err != nil {
				t.Fatal(err)
			}
			filenameCopy := filepath.Join(dir, base)
			if err := os.WriteFile(filenameCopy, data.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			replacements[filename] = filenameCopy
		}
	}
	if sourceDir == "" || len(names) == 0 {
		t.Fatal("no ixgo interpreter tests found")
	}
	sort.Strings(names)
	for name, source := range map[string]string{
		"sandbox_bridge_test.go": bridgeSource,
		"sandbox_main_test.go":   mainSource,
	} {
		filename := filepath.Join(dir, name)
		if err := os.WriteFile(filename, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		replacements[filepath.Join(sourceDir, name)] = filename
	}
	// Go forbids overlays beneath GOMODCACHE. Build and run a writable copy;
	// only its test files are adapted, and production sources remain unchanged.
	work := filepath.Join(dir, "work")
	if err := os.CopyFS(work, os.DirFS(sourceDir)); err != nil {
		t.Fatal(err)
	}
	for original, replacement := range replacements {
		data, err := os.ReadFile(replacement)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, filepath.Base(original)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	modulePath, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	moduleDir := filepath.Dir(strings.TrimSpace(string(modulePath)))
	data, err := os.ReadFile(filepath.Join(moduleDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	mod, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, replacement := range mod.Replace {
		if replacement.New.Version == "" && !filepath.IsAbs(replacement.New.Path) {
			if err := mod.AddReplace(replacement.Old.Path, replacement.Old.Version,
				filepath.Join(moduleDir, replacement.New.Path), ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := mod.AddReplace(path, "", work, ""); err != nil {
		t.Fatal(err)
	}
	data, err = mod.Format()
	if err != nil {
		t.Fatal(err)
	}
	modPath := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(modPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(moduleDir, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), sums, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "ixgo.test")
	cmd := exec.Command("go", "test", "-mod=readonly", "-modfile="+modPath,
		"-ldflags=-checklinkname=0 -s=false -w=false", "-c", "-o", binary, path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ixgo interpreter tests: %v\n%s", err, output)
	}
	t.Logf("%d upstream interpreter tests", len(names))
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			switch name {
			case "TestGoexitDeadlock":
				t.Skip("ixgo retains the host main goroutine ID; Goexit is not supported across migration")
			case "TestRunContext":
				t.Skip("host context.timerCtx/cancelCtx contains process-local cancellation and timer state")
			case "TestTestdataFiles", "TestTestdataFilesRace1", "TestTestdataFilesRace2", "TestTestdataFilesRace3", "TestTestdataFilesRace4", "TestTestdataFilesRace5":
				t.Run("issue5963.go", func(t *testing.T) {
					t.Skip("Goexit fixture excluded from the upstream corpus: ixgo retains the host main goroutine ID")
				})
			case "TestEmbedImethod", "TestStructEmbed":
				t.Skip("test-time ixgo package registration is not transferred to the guest")
			case "TestShadowedMethod":
				// reflectx v1.7.8 drops private field PkgPath when cloning method sets.
				// https://github.com/goplus/reflectx/pull/115
				t.Skip("reflectx loses private field PkgPath, preventing type reconstruction")
			case "TestInterpreter_ConcurrentRun1":
				t.Skip("reflectxtype does not preserve shared methods, exhausting icall slots during concurrent round trips")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.v", "-test.timeout=4m", "-test.run=^"+regexp.QuoteMeta(name)+"$")
			cmd.Dir = work
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("ixgo state round trip: %v\n%s", err, output)
			}
			if bytes.Contains(output, []byte("--- SKIP: "+name+" ")) {
				t.Skipf("upstream skipped:\n%s", output)
			}
			if !bytes.Contains(output, []byte("--- PASS: "+name+" ")) {
				t.Fatalf("upstream test did not report completion:\n%s", output)
			}
			t.Logf("%s", output)
		})
	}
}
