package state

import (
	"context"
	"math"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

func TestSyncMapEntries(t *testing.T) {
	src, dst := new(sync.Map), new(sync.Map)
	entries := map[any]any{
		nil:                    "nil key",
		"nil":                  nil,
		"typed nil":            (*int)(nil),
		42:                     "integer key",
		[2]int{1, 2}:           []int{3, 4},
		"map":                  map[string]int{"n": 5},
		reflect.TypeFor[int](): reflect.TypeFor[string](),
		"reflected":            reflect.ValueOf(7),
	}
	for key, value := range entries {
		src.Store(key, value)
	}
	src.Store("deleted", 1)
	src.Delete("deleted")
	src.Store(math.NaN(), "NaN key")
	dst.Store("old", 1)
	roundtrip(t, src, dst)
	runtime.GC()
	for key, want := range entries {
		got, ok := dst.Load(key)
		if !ok {
			t.Fatalf("missing key %v", key)
		}
		if value, ok := want.(reflect.Value); ok {
			v, ok := got.(reflect.Value)
			if !ok || v.Type() != value.Type() || v.Int() != value.Int() {
				t.Fatalf("reflected value changed: %v", got)
			}
		} else if !reflect.DeepEqual(got, want) {
			t.Fatalf("key %v: got %#v, want %#v", key, got, want)
		}
	}
	count, nan := 0, 0
	dst.Range(func(key, value any) bool {
		count++
		if f, ok := key.(float64); ok && math.IsNaN(f) && value == "NaN key" {
			nan++
		}
		return true
	})
	if count != len(entries)+1 || nan != 1 {
		t.Fatalf("restored entries: count=%d NaN=%d", count, nan)
	}
	if !dst.CompareAndSwap(42, "integer key", "changed") {
		t.Fatal("restored map cannot compare and swap")
	}
	if got, _ := src.Load(42); got != "integer key" {
		t.Fatal("mutating the restored map changed the source")
	}
	if got, loaded := dst.LoadOrStore("added", 9); loaded || got != 9 {
		t.Fatal("restored map cannot add entries")
	}
	if got, loaded := dst.LoadAndDelete("added"); !loaded || got != 9 {
		t.Fatal("restored map cannot delete entries")
	}
	for _, empty := range []*sync.Map{new(sync.Map), src} {
		empty.Clear()
		dst.Store("old", 1)
		roundtrip(t, empty, dst)
		dst.Range(func(key, value any) bool {
			t.Fatalf("empty source retained entry %v: %v", key, value)
			return false
		})
		dst.Store("usable", true)
		if value, ok := dst.Load("usable"); !ok || value != true {
			t.Fatal("empty restored map is not usable")
		}
	}
}

func TestSyncMapGraph(t *testing.T) {
	type root struct {
		cache sync.Map
		Alias *sync.Map
		Other *sync.Map
		Node  *graphNode
		Slice []int
		Map   map[string]any
	}
	src := &root{
		Other: new(sync.Map),
		Node:  &graphNode{Value: 42},
		Slice: []int{1, 2},
		Map:   make(map[string]any),
	}
	src.Alias = &src.cache
	src.Node.Next = src.Node
	src.cache.Store("self", &src.cache)
	src.cache.Store("other", src.Other)
	src.cache.Store(src.Node, &src.Node.Value)
	src.cache.Store("slice", src.Slice)
	src.cache.Store("map", src.Map)
	src.Other.Store("parent", &src.cache)
	src.Map["parent"] = &src.cache
	dst := new(root)
	roundtrip(t, src, dst)
	runtime.GC()
	if dst.Alias != &dst.cache || dst.Other == src.Other || dst.Node == src.Node || dst.Node.Next != dst.Node || !dst.Node.loaded {
		t.Fatal("map or value alias, cycle, or load hook was lost")
	}
	if got, _ := dst.cache.Load("self"); got != &dst.cache {
		t.Fatal("map self-reference was lost")
	}
	if got, _ := dst.cache.Load("other"); got != dst.Other {
		t.Fatal("map reference was lost")
	}
	if got, _ := dst.Other.Load("parent"); got != &dst.cache || dst.Map["parent"] != &dst.cache {
		t.Fatal("mutual map references were lost")
	}
	if got, ok := dst.cache.Load(dst.Node); !ok || got != &dst.Node.Value {
		t.Fatal("pointer key or interior pointer value was lost")
	}
	if _, ok := dst.cache.Load(src.Node); ok {
		t.Fatal("map retained the source pointer key")
	}
	slice, _ := dst.cache.Load("slice")
	slice.([]int)[0] = 8
	m, _ := dst.cache.Load("map")
	m.(map[string]any)["added"] = 9
	if dst.Slice[0] != 8 || src.Slice[0] != 1 || dst.Map["added"] != 9 || src.Map["added"] != nil {
		t.Fatal("map or slice values lost aliasing or isolation")
	}
}

func TestSyncMapRoundTrip(t *testing.T) {
	type root struct {
		Map, Alias, Detached *sync.Map
		Node, Added          *graphNode
	}
	m, detached := new(sync.Map), new(sync.Map)
	node := &graphNode{Value: 1}
	m.Store(node, node)
	m.Store("delete", true)
	detached.Store("value", 1)
	host := root{Map: m, Alias: m, Detached: detached, Node: node}
	var guest root
	var source, destination State
	ctx := context.Background()
	mem := make([]byte, 1<<20)
	for i := 0; i < 3; i++ {
		n, _, err := source.Save(ctx, mem, &host)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := destination.Load(ctx, mem[:n], &guest); err != nil {
			t.Fatal(err)
		}
		if guest.Map == m || guest.Map != guest.Alias || guest.Node == node {
			t.Fatal("guest lost map aliases or reused host memory")
		}
		guest.Node.Value++
		guest.Map.Delete("delete")
		guest.Added = &graphNode{Value: int64(i + 10), Next: guest.Node}
		guest.Map.Store("added", guest.Added)
		guest.Map.Store(guest.Map, guest.Node)
		if guest.Detached != nil {
			guest.Detached.Store("value", 2)
			guest.Detached = nil
		}
		n, _, err = destination.Save(ctx, mem, &guest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.Load(ctx, mem[:n-1], &host); err == nil {
			t.Fatal("truncated return accepted")
		}
		if host.Map != m || node.Value != int64(i+1) {
			t.Fatal("rejected return modified the host graph")
		}
		if i == 0 {
			if value, ok := m.Load("delete"); !ok || value != true {
				t.Fatal("rejected return removed an original map entry")
			}
		}
		if _, err := source.Load(ctx, mem[:n], &host); err != nil {
			t.Fatal(err)
		}
		if host.Map != m || host.Alias != m || host.Node != node || node.Value != int64(i+2) || host.Detached != nil {
			t.Fatal("writeback replaced original objects or lost changes")
		}
		if value, ok := m.Load(node); !ok || value != node {
			t.Fatal("writeback lost the original pointer key")
		}
		if value, ok := m.Load(m); !ok || value != node {
			t.Fatal("writeback lost the map pointer key")
		}
		if _, ok := m.Load("delete"); ok {
			t.Fatal("writeback did not remove a deleted entry")
		}
		if value, ok := m.Load("added"); !ok || value != host.Added || host.Added == guest.Added || host.Added.Next != node || host.Added.Value != int64(i+10) {
			t.Fatal("writeback lost the new object or its aliases")
		}
		if value, _ := detached.Load("value"); value != 2 {
			t.Fatal("writeback lost changes to an unlinked map")
		}
	}
}
