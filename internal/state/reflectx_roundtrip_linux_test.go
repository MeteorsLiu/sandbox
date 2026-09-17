//go:build linux && (amd64 || arm64)

package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goplus/reflectx"
)

func TestReflectxRoundTripTypeIdentity(t *testing.T) {
	const imageEnv = "SANDBOX_REFLECTX_ROUNDTRIP_IMAGE"
	ctx := context.Background()
	mem := make([]byte, 16<<20)
	if path := os.Getenv(imageEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var graph State
		var root methodRoot
		if _, err := graph.Load(ctx, data, &root); err != nil {
			t.Fatal(err)
		}
		checkMethodRoot(t, root)
		n, _, err := graph.Save(ctx, mem, &root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, mem[:n], 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if os.Getenv("SANDBOX_REFLECTX_ROUNDTRIP_SOURCE") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestReflectxRoundTripTypeIdentity$", "-test.v")
		command.Env = append(os.Environ(), "SANDBOX_REFLECTX_ROUNDTRIP_SOURCE=1")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("source process: %v\n%s", err, out)
		}
		return
	}
	root := methodFixture()
	originalType, originalObject := root.Type, root.Object
	originalCounter := root.Counter
	var graph State
	n, _, err := graph.Save(ctx, mem, &root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, mem[:n], 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestReflectxRoundTripTypeIdentity$", "-test.v")
	command.Env = append(os.Environ(), imageEnv+"="+path)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("guest process: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, allocated, _ := reflectx.IcallStat()
	if _, err := graph.Load(ctx, data, &root); err != nil {
		t.Fatal(err)
	}
	if root.Type != originalType || root.Object != originalObject || root.Counter != originalCounter || reflect.TypeOf(root.Object).Elem() != originalType {
		t.Fatal("return replaced a retained type, object or capture")
	}
	if *originalCounter != 4 || root.Number.Number() != 21 || originalObject.(methodNumber).Number() != 21 {
		t.Fatal("retained methods do not see returned captures and receiver")
	}
	_, after, _ := reflectx.IcallStat()
	if after != allocated {
		t.Fatalf("return allocated method slots: before=%d after=%d", allocated, after)
	}
}
