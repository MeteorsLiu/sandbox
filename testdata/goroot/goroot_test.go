//go:build linux && (amd64 || arm64) && cgo

package goroot_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"go/build"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goplus/ixgo"
	_ "github.com/goplus/ixgo/pkg"
	"github.com/xgo-dev/sandbox"
)

var (
	gorootFile        = flag.String("goroot-file", "", "run one Go source file through ixgo and Sentry")
	gorootCaseTimeout = flag.Duration("goroot-case-timeout", 2*time.Minute, "timeout for each GOROOT test, including source generation")
)

func TestMain(m *testing.M) {
	flag.Parse()
	if *gorootFile != "" {
		// A helper process prints only the program's output, without test PASS lines.
		code, err := runProgram(*gorootFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func runProgram(path string) (int, error) {
	library := os.Getenv("SANDBOX_TEST_LIBRARY")
	if library == "" {
		return 0, fmt.Errorf("SANDBOX_TEST_LIBRARY must point to the matching Sentry shared library")
	}
	// Match ixgo run's arguments before init and again in the fresh guest.
	dir, _ := filepath.Split(path)
	os.Args = []string{dir}
	interp, err := ixgo.NewContext(ixgo.ExperimentalSupportGC|ixgo.SupportMultipleInterp).LoadInterp(path, nil)
	if err != nil {
		return 0, fmt.Errorf("load: %w", err)
	}
	defer interp.UnsafeRelease()
	if err := interp.RunInit(); err != nil {
		return 0, fmt.Errorf("init: %w", err)
	}

	var code int
	var runError string
	var completed bool
	s := sandbox.Sandbox{Library: library}
	defer s.Close()
	err = s.Run(func() {
		os.Args = []string{dir}
		var err error
		code, err = interp.RunMain()
		if err != nil {
			runError = fmt.Sprint(err)
		}
		completed = true
	})
	if err != nil {
		return 0, fmt.Errorf("sandbox round trip: %w", err)
	}
	if !completed {
		return 0, fmt.Errorf("guest completion was not written back")
	}
	if runError != "" {
		return code, fmt.Errorf("main: %s", runError)
	}
	return code, nil
}

// TestGOROOT adapts ixgo's cmd/ixgotest, which runs $GOROOT/test language
// regression programs, not the standard library's src/* unit tests.
// Build with -ldflags='-checklinkname=0 -s=false -w=false', then run the
// executable with SANDBOX_TEST_LIBRARY set and GLIBC_TUNABLES=glibc.pthread.rseq=0.
func TestGOROOT(t *testing.T) {
	if os.Getenv("SANDBOX_TEST_LIBRARY") == "" {
		t.Fatal("SANDBOX_TEST_LIBRARY must point to the matching Sentry shared library")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(build.Default.GOROOT, "test")
	type testFile struct {
		path      string
		runOutput bool
	}
	var runs, generators []testFile
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == root {
				return nil
			}
			switch entry.Name() {
			case "bench", "dwarf", "codegen":
				return filepath.SkipDir
			case "typeparam":
				switch runtime.Version()[:6] {
				case "go1.18", "go1.19", "go1.20":
				default:
					return filepath.SkipDir
				}
			default:
				if strings.Contains(entry.Name(), ".dir") {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		line, _, _ := strings.Cut(string(data), "\n")
		switch strings.TrimSpace(line) {
		case "// run", "// run -gcflags=-G=3":
			runs = append(runs, testFile{path: path})
		case "// runoutput":
			generators = append(generators, testFile{path: path, runOutput: true})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs)+len(generators) == 0 {
		t.Fatalf("no ixgotest cases found in %s", root)
	}
	t.Logf("%s: %d run, %d runoutput cases before upstream skips", root, len(runs), len(generators))
	for _, file := range append(runs, generators...) {
		name, err := filepath.Rel(root, file.path)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.ToSlash(name), func(t *testing.T) {
			if reason, ok := gorootTestSkips[name]; ok {
				t.Skip(reason)
			}
			ctx, cancel := context.WithTimeout(t.Context(), *gorootCaseTimeout)
			defer cancel()
			input := file.path
			if file.runOutput {
				data, err := exec.CommandContext(ctx, "go", "run", input).CombinedOutput()
				if err != nil {
					t.Fatalf("runoutput generator: %v (context: %v)\n%s", err, ctx.Err(), data)
				}
				input = filepath.Join(t.TempDir(), "main.go")
				if err := os.WriteFile(input, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			output, err := exec.CommandContext(ctx, executable, "-goroot-file="+input).CombinedOutput()
			if err != nil {
				t.Fatalf("ixgo/Sentry: %v (context: %v)\n%s", err, ctx.Err(), output)
			}
			if bytes.Contains(output, []byte("BUG")) {
				t.Fatalf("program reported BUG:\n%s", output)
			}
			if !file.runOutput {
				golden := strings.TrimSuffix(input, ".go") + ".out"
				want, err := os.ReadFile(golden)
				switch {
				case err == nil:
					if !bytes.Equal(output, want) {
						t.Fatalf("output mismatch\ngot:  %q\nwant: %q", output, want)
					}
				case !os.IsNotExist(err):
					t.Fatal(err)
				}
			}
			if len(output) != 0 {
				t.Logf("%s", output)
			}
		})
	}
}
