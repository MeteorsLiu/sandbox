package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goplus/ixgo"
)

var (
	variableNumber       = os.Getpid()
	variableSecret       = "native variable contents must not enter the image"
	variableSentinel     = errors.New("receiver-owned sentinel")
	unregisteredVariable = 71
)

func init() {
	ixgo.RegisterPackage(&ixgo.Package{
		Name: "variables",
		Path: "sandbox/state/testvariables",
		Vars: map[string]reflect.Value{
			"Number":   reflect.ValueOf(&variableNumber),
			"Secret":   reflect.ValueOf(&variableSecret),
			"Sentinel": reflect.ValueOf(&variableSentinel),
		},
	})
}

func TestIxgoVariableRoundTrip(t *testing.T) {
	original := variableNumber
	defer func() { variableNumber = original }()
	type root struct {
		Pointers     []*int
		Keys         map[*int]string
		Any          any
		Reflected    reflect.Value
		Addressable  reflect.Value
		Secret       *string
		Sentinel     *error
		Unregistered *int
	}
	n := 23
	host := root{
		Pointers:     []*int{&variableNumber, nil, &n, &variableNumber},
		Keys:         map[*int]string{&variableNumber: "native", &n: "ordinary"},
		Any:          &variableNumber,
		Reflected:    reflect.ValueOf(&variableNumber),
		Addressable:  reflect.ValueOf(&variableNumber).Elem(),
		Secret:       &variableSecret,
		Sentinel:     &variableSentinel,
		Unregistered: &unregisteredVariable,
	}
	ctx := context.Background()
	mem := make([]byte, 1<<20)
	var source, destination State
	size, _, err := source.Save(ctx, mem, &host)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(mem[:size], []byte(variableSecret)) {
		t.Fatal("registered variable contents entered the image")
	}
	variableNumber = 42
	var guest root
	if _, err := destination.Load(ctx, mem[:size], &guest); err != nil {
		t.Fatal(err)
	}
	if variableNumber != 42 || guest.Pointers[0] != &variableNumber || guest.Pointers[1] != nil || guest.Pointers[3] != &variableNumber || guest.Any != &variableNumber {
		t.Fatal("native variable aliases were copied or overwritten")
	}
	if guest.Keys[&variableNumber] != "native" || guest.Keys[guest.Pointers[2]] != "ordinary" || guest.Pointers[2] == &n || *guest.Pointers[2] != n {
		t.Fatal("mixed native and ordinary pointer keys lost their identity")
	}
	if guest.Reflected.Interface() != &variableNumber || guest.Addressable.Addr().Interface() != &variableNumber {
		t.Fatal("reflect.Value did not bind to local variable storage")
	}
	if guest.Secret != &variableSecret || guest.Sentinel != &variableSentinel || *guest.Sentinel != variableSentinel {
		t.Fatal("registered string or error variable was copied")
	}
	if guest.Unregistered == &unregisteredVariable || *guest.Unregistered != unregisteredVariable {
		t.Fatal("unregistered global changed its ordinary object semantics")
	}
	*guest.Pointers[2] = 99
	guest.Addressable.SetInt(43)
	size, _, err = destination.Save(ctx, mem, &guest)
	if err != nil {
		t.Fatal(err)
	}
	variableNumber = 44
	if _, err := source.Load(ctx, mem[:size], &host); err != nil {
		t.Fatal(err)
	}
	if variableNumber != 44 || host.Pointers[0] != &variableNumber || host.Pointers[2] != &n || n != 99 {
		t.Fatal("return load overwrote native storage or lost ordinary writeback")
	}
}

func TestIxgoVariableNewProcess(t *testing.T) {
	const imageEnv = "SANDBOX_STATE_VARIABLE_TEST_IMAGE"
	type root struct {
		Number   *int
		Sentinel *error
		Result   int
	}
	ctx := context.Background()
	mem := make([]byte, 1<<20)
	if path := os.Getenv(imageEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var state State
		var guest root
		if _, err := state.Load(ctx, data, &guest); err != nil {
			t.Fatal(err)
		}
		if guest.Number != &variableNumber || *guest.Number != os.Getpid() || guest.Sentinel != &variableSentinel || *guest.Sentinel != variableSentinel {
			t.Fatal("references did not use child package initialization")
		}
		*guest.Number = -1
		guest.Result = 123
		n, _, err := state.Save(ctx, mem, &guest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".return", mem[:n], 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	var state State
	host := root{Number: &variableNumber, Sentinel: &variableSentinel}
	n, _, err := state.Save(ctx, mem, &host)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "variables.state")
	if err := os.WriteFile(path, mem[:n], 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestIxgoVariableNewProcess$")
	cmd.Env = append(os.Environ(), imageEnv+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}
	data, err := os.ReadFile(path + ".return")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Load(ctx, data, &host); err != nil {
		t.Fatal(err)
	}
	if variableNumber != os.Getpid() || host.Number != &variableNumber || host.Sentinel != &variableSentinel || host.Result != 123 {
		t.Fatal("return failed to preserve host native state and copy the result")
	}
}

func TestIxgoVariableInvalidReference(t *testing.T) {
	valid := packageVariable{pkg: "sandbox/state/testvariables", name: "Number"}
	for _, test := range []struct {
		name string
		ref  refValue
		out  any
		want string
	}{
		{"package", refValue{variable: &packageVariable{pkg: "sandbox/state/missing", name: "Number"}}, new(*int), "not registered"},
		{"variable", refValue{variable: &packageVariable{pkg: valid.pkg, name: "Missing"}}, new(*int), "not registered"},
		{"type", refValue{variable: &valid}, new(*string), "pointer type"},
		{"kind", refValue{variable: &valid}, new(int), "cannot be decoded"},
		{"object", refValue{Root: 1, variable: &valid}, new(*int), "invalid ixgo variable reference"},
		{"path", refValue{Dots: []dot{index(0)}, variable: &valid}, new(*int), "invalid ixgo variable reference"},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := writer{mem: make([]byte, 256)}
			if err := w.put(&test.ref); err != nil {
				t.Fatal(err)
			}
			ds := newDecodeState(context.Background(), w.mem[:w.pos])
			err := safely(func() {
				obj := loadObject(&ds.r)
				ds.decodeObject(nil, reflect.ValueOf(test.out).Elem(), obj)
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}
