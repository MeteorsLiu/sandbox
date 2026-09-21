package state

import (
	"bytes"
	"context"
	"runtime"
	"testing"
	"weak"
)

func TestStateReleasesTransferResources(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "Save"
		if stream {
			name = "SaveTo"
		}
		t.Run(name, func(t *testing.T) {
			value := 10
			host := [2]*int{&value, &value}
			var source, destination State
			data, resources := saveWithResources(t, &source, &host, stream)
			runtime.GC()
			for _, pointer := range resources {
				if pointer.Value() != nil {
					t.Fatal("Save retained its context or output buffer")
				}
			}
			var guest [2]*int
			resources = loadWithResources(t, &destination, data, &guest)
			runtime.GC()
			for _, pointer := range resources {
				if pointer.Value() != nil {
					t.Fatal("Load retained its context or input buffer")
				}
			}
			if guest[0] == &value || guest[0] != guest[1] || *guest[0] != 10 {
				t.Fatal("Load lost isolation or aliases")
			}
			*guest[0] = 20
			guest[0], guest[1] = nil, nil
			data, resources = saveWithResources(t, &destination, &guest, stream)
			runtime.GC()
			for _, pointer := range resources {
				if pointer.Value() != nil {
					t.Fatal("return Save retained its context or output buffer")
				}
			}
			resources = loadWithResources(t, &source, data, &host)
			runtime.GC()
			for _, pointer := range resources {
				if pointer.Value() != nil {
					t.Fatal("writeback retained its context or input buffer")
				}
			}
			if host != [2]*int{} || value != 20 {
				t.Fatal("writeback lost a detached object's identity")
			}
			runtime.KeepAlive(&source)
			runtime.KeepAlive(&destination)
		})
	}
}

// Keep the caller's resources outside the frame that runs GC, so compiler
// liveness cannot mask references retained by State itself.
//
//go:noinline
func saveWithResources(t *testing.T, graph *State, root any, stream bool) ([]byte, []weak.Pointer[byte]) {
	t.Helper()
	marker := make([]byte, 4096)
	ctx := context.WithValue(context.Background(), struct{}{}, marker)
	resources := []weak.Pointer[byte]{weak.Make(&marker[0])}
	var data []byte
	if stream {
		var out bytes.Buffer
		if _, _, err := graph.SaveTo(ctx, &out, root); err != nil {
			t.Fatal(err)
		}
		data = out.Bytes()
	} else {
		mem := make([]byte, 1<<20)
		n, _, err := graph.Save(ctx, mem, root)
		if err != nil {
			t.Fatal(err)
		}
		data = mem[:n]
	}
	resources = append(resources, weak.Make(&data[0]))
	return bytes.Clone(data), resources
}

//go:noinline
func loadWithResources(t *testing.T, graph *State, data []byte, root any) []weak.Pointer[byte] {
	t.Helper()
	marker := make([]byte, 4096)
	ctx := context.WithValue(context.Background(), struct{}{}, marker)
	input := bytes.Clone(data)
	resources := []weak.Pointer[byte]{weak.Make(&marker[0]), weak.Make(&input[0])}
	if _, err := graph.Load(ctx, input, root); err != nil {
		t.Fatal(err)
	}
	return resources
}
