package state

import (
	"context"
	"go/token"
	"go/types"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"golang.org/x/tools/go/types/typeutil"
)

type copiedTypeMap typeutil.Map

func (m *copiedTypeMap) Iterate(f func(types.Type, any)) {
	(*typeutil.Map)(m).Iterate(f)
}

func (m *copiedTypeMap) Set(key types.Type, value any) any {
	return (*typeutil.Map)(m).Set(key, value)
}

func TestTypeutilMapPattern(t *testing.T) {
	type root struct {
		cache copiedTypeMap
		Copy  any
		Key   *types.Named
	}
	src := &root{Key: types.NewNamed(types.NewTypeName(token.NoPos, nil, "Node", nil), types.Typ[types.Int], nil)}
	src.cache.Set(src.Key, &src.cache)
	src.Copy = src.cache
	dst := new(root)
	roundtrip(t, src, dst)
	if dst.Key == src.Key || (*typeutil.Map)(&dst.cache).At(dst.Key) != &dst.cache {
		t.Fatal("matching map lost its relocated key or self-reference")
	}
	copy := dst.Copy.(copiedTypeMap)
	dst.cache.Set(dst.Key, "updated")
	if (*typeutil.Map)(&copy).At(dst.Key) != "updated" {
		t.Fatal("matching map lost its shared table")
	}
	if (*typeutil.Map)(&src.cache).At(src.Key) != &src.cache {
		t.Fatal("restoring changed the source map")
	}
}

func TestTypeutilMapEntries(t *testing.T) {
	named := types.NewNamed(types.NewTypeName(token.NoPos, nil, "Node", nil), types.NewStruct(nil, nil), nil)
	named.SetUnderlying(types.NewStruct([]*types.Var{
		types.NewField(token.NoPos, nil, "Next", types.NewPointer(named), false),
	}, nil))
	keys := []types.Type{
		named,
		types.NewPointer(named),
		types.NewSlice(named),
		types.NewArray(named, 3),
		types.NewMap(named, types.Typ[types.Int]),
		types.NewChan(types.SendRecv, named),
		types.NewSignatureType(nil, nil, nil, types.NewTuple(types.NewVar(token.NoPos, nil, "n", named)), nil, false),
	}
	type root struct {
		Map  typeutil.Map
		Keys []types.Type
	}
	src := &root{Keys: keys}
	for i, key := range keys {
		src.Map.Set(key, i)
	}
	src.Map.Set(types.Typ[types.String], "deleted")
	src.Map.Delete(types.Typ[types.String])
	dst := new(root)
	dst.Map.Set(types.Typ[types.Bool], "old")
	roundtrip(t, src, dst)
	runtime.GC()
	if dst.Map.Len() != len(keys) || dst.Keys[0] == named {
		t.Fatal("entries lost or named key not relocated")
	}
	for i, key := range dst.Keys {
		if got := dst.Map.At(key); got != i {
			t.Fatalf("key %d (%T): got %v, want %d", i, key, got, i)
		}
	}
	if dst.Map.At(types.NewPointer(dst.Keys[0])) != 1 {
		t.Fatal("structurally identical key is not found")
	}
	var readers sync.WaitGroup
	for range 10 {
		readers.Go(func() {
			for range 100 {
				for i, key := range dst.Keys {
					if got := dst.Map.At(key); got != i {
						t.Errorf("concurrent lookup %d: got %v", i, got)
					}
				}
			}
		})
	}
	readers.Wait()
	if old := dst.Map.Set(dst.Keys[0], "changed"); old != 0 || dst.Map.Len() != len(keys) {
		t.Fatal("Set inserted an existing key twice")
	}
	if !dst.Map.Delete(dst.Keys[0]) || dst.Map.Len() != len(keys)-1 || src.Map.At(named) != 0 {
		t.Fatal("Delete failed or changed the source")
	}
	for _, empty := range []*typeutil.Map{new(typeutil.Map), &dst.Map} {
		empty.Iterate(func(key types.Type, _ any) { empty.Delete(key) })
		var restored typeutil.Map
		restored.Set(types.Typ[types.Bool], true)
		roundtrip(t, empty, &restored)
		if restored.Len() != 0 || restored.At(types.Typ[types.Bool]) != nil {
			t.Fatal("empty map retained old entries")
		}
		restored.Set(types.Typ[types.Int], 42)
		if restored.At(types.Typ[types.Int]) != 42 {
			t.Fatal("empty restored map is not usable")
		}
	}
}

func TestTypeutilMapAliases(t *testing.T) {
	type root struct {
		Map    typeutil.Map
		Alias  *typeutil.Map
		Other  *typeutil.Map
		Copies []any
		Key    *types.Named
		Node   *graphNode
	}
	src := &root{
		Other: new(typeutil.Map),
		Key:   types.NewNamed(types.NewTypeName(token.NoPos, nil, "Node", nil), types.Typ[types.Int], nil),
		Node:  &graphNode{Value: 42},
	}
	src.Alias = &src.Map
	src.Map.Set(src.Key, src.Node)
	src.Map.Set(types.Typ[types.Int], src.Other)
	src.Other.Set(src.Key, &src.Map)
	src.Copies = []any{src.Map, map[string]typeutil.Map{"copy": src.Map}}
	dst := new(root)
	roundtrip(t, src, dst)
	if dst.Alias != &dst.Map || dst.Key == src.Key || dst.Node == src.Node || !dst.Node.loaded {
		t.Fatal("map aliases, moved objects, or load callback lost")
	}
	if dst.Map.At(types.Typ[types.Int]) != dst.Other || dst.Other.At(dst.Key) != &dst.Map {
		t.Fatal("mutual references lost")
	}
	copies := []typeutil.Map{dst.Map, dst.Copies[0].(typeutil.Map), dst.Copies[1].(map[string]typeutil.Map)["copy"]}
	for i := range copies {
		if copies[i].At(dst.Key) != dst.Node {
			t.Fatalf("copy %d retained stale buckets", i)
		}
	}
	dst.Map.Set(dst.Key, "updated")
	for i := range copies {
		if copies[i].At(dst.Key) != "updated" {
			t.Fatalf("copy %d lost its shared table", i)
		}
	}
	if src.Map.At(src.Key) != src.Node {
		t.Fatal("restoring changed the source map")
	}
}

func TestTypeutilMapRoundTrip(t *testing.T) {
	type root struct {
		Map, Alias *typeutil.Map
		Key        *types.Named
		Node       *graphNode
	}
	m := new(typeutil.Map)
	key := types.NewNamed(types.NewTypeName(token.NoPos, nil, "Node", nil), types.Typ[types.Int], nil)
	node := &graphNode{Value: 1}
	m.Set(key, node)
	host := root{Map: m, Alias: m, Key: key, Node: node}
	var guest root
	var source, destination State
	mem := make([]byte, 1<<20)
	ctx := context.Background()
	for i := range 3 {
		n, _, err := source.Save(ctx, mem, &host)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := destination.Load(ctx, mem[:n], &guest); err != nil {
			t.Fatal(err)
		}
		if guest.Map == m || guest.Map != guest.Alias || guest.Key == key || guest.Map.At(guest.Key) != guest.Node {
			t.Fatal("guest lost aliases or retained host objects")
		}
		guest.Node.Value++
		guest.Map.Set(types.NewSlice(guest.Key), reflect.TypeFor[int]())
		n, _, err = destination.Save(ctx, mem, &guest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.Load(ctx, mem[:n], &host); err != nil {
			t.Fatal(err)
		}
		if host.Map != m || host.Alias != m || host.Key != key || host.Node != node || node.Value != int64(i+2) {
			t.Fatal("writeback replaced original objects or lost changes")
		}
		if m.At(key) != node || m.At(types.NewSlice(key)) != reflect.TypeFor[int]() || m.Len() != 2 {
			t.Fatal("writeback retained guest hashes")
		}
	}
}
