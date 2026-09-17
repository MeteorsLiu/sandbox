//go:build linux && (amd64 || arm64)

package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goplus/ixgo"
)

var ixgoPackagePID = os.Getpid()

var ixgoTestPackage = &ixgo.Package{
	Name: "statefixture",
	Path: "sandbox/state/testpackage",
	Vars: map[string]reflect.Value{
		"PID":                 reflect.ValueOf(&ixgoPackagePID),
		"ErrDeadlineExceeded": reflect.ValueOf(&os.ErrDeadlineExceeded),
	},
}

func init() { ixgo.RegisterPackage(ixgoTestPackage) }

func TestIxgoPackageRoundTrip(t *testing.T) {
	type root struct {
		Package   *ixgo.Package
		Packages  []*ixgo.Package
		Installed map[string]*ixgo.Package
		Any       any
		Reflected reflect.Value
	}
	host := root{
		Package:   ixgoTestPackage,
		Packages:  []*ixgo.Package{nil, ixgoTestPackage, nil, ixgoTestPackage},
		Installed: map[string]*ixgo.Package{"nil": nil, "pkg": ixgoTestPackage},
		Any:       ixgoTestPackage,
		Reflected: reflect.ValueOf(ixgoTestPackage),
	}
	var guest root
	var source, destination State
	ctx := context.Background()
	mem := make([]byte, 1<<20)
	for i := 0; i < 2; i++ {
		n, _, err := source.Save(ctx, mem, &host)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := destination.Load(ctx, mem[:n], &guest); err != nil {
			t.Fatal(err)
		}
		if guest.Package != ixgoTestPackage || guest.Packages[0] != nil || guest.Packages[1] != ixgoTestPackage || guest.Packages[2] != nil || guest.Packages[3] != ixgoTestPackage || guest.Installed["nil"] != nil || guest.Installed["pkg"] != ixgoTestPackage || guest.Any != ixgoTestPackage || guest.Reflected.Interface() != ixgoTestPackage {
			t.Fatal("package references were not rebound to the local registry")
		}
		n, _, err = destination.Save(ctx, mem, &guest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.Load(ctx, mem[:n], &host); err != nil {
			t.Fatal(err)
		}
		if host.Package != ixgoTestPackage || host.Any != ixgoTestPackage {
			t.Fatal("return load replaced the registered package")
		}
	}
}

func TestIxgoPackageMissing(t *testing.T) {
	ctx := context.Background()
	mem := make([]byte, 1<<20)
	src := &ixgo.Package{Path: "sandbox/state/not-registered"}
	n, _, err := Save(ctx, mem, &src)
	if err != nil {
		t.Fatal(err)
	}
	var dst *ixgo.Package
	if _, err := Load(ctx, mem[:n], &dst); err == nil || !strings.Contains(err.Error(), "is not registered") {
		t.Fatalf("unregistered package: %v", err)
	}
	src = &ixgo.Package{}
	if _, _, err := Save(ctx, mem, &src); err == nil || !strings.Contains(err.Error(), "no package path") {
		t.Fatalf("empty package path: %v", err)
	}
}

func TestIxgoPackageNewProcess(t *testing.T) {
	const imageEnv = "SANDBOX_STATE_PACKAGE_TEST_IMAGE"
	if path := os.Getenv(imageEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var restored *ixgo.Package
		if _, err := Load(context.Background(), data, &restored); err != nil {
			t.Fatal(err)
		}
		if restored != ixgoTestPackage || restored.Vars["PID"].Elem().Int() != int64(os.Getpid()) || restored.Vars["ErrDeadlineExceeded"].Elem().Interface() != os.ErrDeadlineExceeded {
			t.Fatal("package data was copied instead of using the child's init registration")
		}
		return
	}
	mem := make([]byte, 1<<20)
	pkg := ixgoTestPackage
	n, _, err := Save(context.Background(), mem, &pkg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "package.state")
	if err := os.WriteFile(path, mem[:n], 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestIxgoPackageNewProcess$")
	cmd.Env = append(os.Environ(), imageEnv+"="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}
