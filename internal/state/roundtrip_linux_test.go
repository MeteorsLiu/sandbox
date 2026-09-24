//go:build linux && (amd64 || arm64)

package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

type objectIDFunctions struct {
	Native    func() int
	Wrapped   func() int
	Alias     func() int
	Method    func(int) int
	Once      func() *nativeGenericNode
	OnceAlias func() *nativeGenericNode
	OnceCalls int
}

//go:noinline
func objectIDMakeFunc(n *int) func() int {
	callback := func([]reflect.Value) []reflect.Value {
		*n += 2
		return []reflect.Value{reflect.ValueOf(*n)}
	}
	return reflect.MakeFunc(reflect.TypeFor[func() int](), callback).Interface().(func() int)
}

func TestObjectIDClosureProcess(t *testing.T) {
	const imageEnv = "SANDBOX_STATE_OBJECTID_IMAGE"
	ctx := context.Background()
	if path := os.Getenv(imageEnv); path != "" {
		input, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var guest objectIDFunctions
		loaded := newDecodeState(ctx, input)
		loadObjects(t, loaded, &guest)
		if guest.Native() != 11 || guest.Wrapped() != 13 || guest.Alias() != 15 || guest.Method(3) != 23 {
			t.Fatal("guest functions lost their shared captures or receiver")
		}
		node := guest.Once()
		if node.N != 42 || node.Next != node || guest.OnceAlias() != node || guest.OnceCalls != 1 {
			t.Fatal("guest OnceValue lost its captures, cached result or alias")
		}
		node.N++
		guest.Native = nil
		runtime.GC()
		returned := loaded.loaded().encoder(ctx, make([]byte, 1<<20))
		output := saveObjects(t, returned, &guest)
		if returned.lastID != objectID(len(loaded.objectsByID)) {
			t.Fatal("native or MakeFunc environments acquired new IDs")
		}
		if err := os.WriteFile(path, output, 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	n := 10
	receiver := &nativeMethodReceiver{N: 20}
	fn := objectIDMakeFunc(&n)
	host := objectIDFunctions{Native: nativeELFClosure(&n), Wrapped: fn, Alias: fn, Method: receiver.Add}
	node := &nativeGenericNode{N: 42}
	node.Next = node
	host.Once = sync.OnceValue(func() *nativeGenericNode { host.OnceCalls++; return node })
	host.OnceAlias = host.Once
	originalOnce := host.Once
	original := host.Native
	saved := newEncodeState(ctx, make([]byte, 1<<20))
	input := saveObjects(t, saved, &host)
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, input, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestObjectIDClosureProcess$")
	command.Env = append(os.Environ(), imageEnv+"="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("guest process: %v\n%s", err, output)
	}
	if n != 10 || receiver.N != 20 || node.N != 42 || host.OnceCalls != 0 {
		t.Fatal("guest changed host memory before writeback")
	}
	output, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loadObjects(t, saved.saved().decoder(ctx, output), &host)
	runtime.GC()
	if host.Once() != node || host.OnceAlias() != node || originalOnce() != node || node.N != 43 || node.Next != node || host.OnceCalls != 1 {
		t.Fatal("OnceValue writeback lost its original objects or completion state")
	}
	if host.Native != nil || n != 15 || receiver.N != 23 || original() != 16 || host.Wrapped() != 18 || host.Alias() != 20 || fn() != 22 || host.Method(1) != 24 {
		t.Fatalf("host captures did not preserve identity: n=%d receiver=%d", n, receiver.N)
	}
	if makeFuncCallback(reflect.ValueOf(host.Wrapped)).Addr().Pointer() != makeFuncCallback(reflect.ValueOf(host.Alias)).Addr().Pointer() {
		t.Fatal("MakeFunc aliases were split on return")
	}
}
