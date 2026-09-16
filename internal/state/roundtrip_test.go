package state

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func saveObjects(t *testing.T, es *encodeState, root any) []byte {
	t.Helper()
	if err := safely(func() { es.Save(reflect.ValueOf(root).Elem()) }); err != nil {
		t.Fatal(err)
	}
	return es.w.mem[:es.w.pos]
}

func loadObjects(t *testing.T, ds *decodeState, root any) {
	t.Helper()
	if err := safely(func() { ds.Load(reflect.ValueOf(root).Elem()) }); err != nil {
		t.Fatal(err)
	}
}

func TestObjectIDRoundTrip(t *testing.T) {
	type root struct {
		A, B, Dropped, New *graphNode
		Map, MapAlias      map[string]*graphNode
		Slice, SliceAlias  []int
		Field              *int64
		Value              reflect.Value
	}
	a, b, dropped := &graphNode{Value: 10}, &graphNode{Value: 10}, &graphNode{Value: 30}
	a.Next, b.Next = b, a
	m := map[string]*graphNode{"keep": a, "delete": b}
	array := [3]int{1, 2, 3}
	host := root{a, b, dropped, nil, m, m, array[:], array[:2], &a.Value, reflect.ValueOf(a).Elem().Field(0)}
	ctx := context.Background()
	saved := newEncodeState(ctx, make([]byte, 1<<20))
	input := saveObjects(t, saved, &host)
	var guest root
	loaded := newDecodeState(ctx, input)
	loadObjects(t, loaded, &guest)
	if guest.A == a || guest.B == b || guest.Field == &a.Value || &guest.Slice[0] == &array[0] {
		t.Fatal("guest reused host memory")
	}
	guest.A, guest.B = guest.B, guest.A
	guest.A.Value, guest.B.Value = 20, 11
	guest.Dropped.Value = 31
	guest.Dropped = nil
	guest.New = &graphNode{Value: 40, Next: guest.B}
	delete(guest.Map, "delete")
	guest.Map["new"] = guest.New
	guest.Map, guest.MapAlias = nil, nil
	guest.SliceAlias[1] = 8
	guest.Slice = append(guest.Slice, 4)
	guest.Value.SetInt(12)
	runtime.GC()
	returned := loaded.encoder(ctx, make([]byte, 1<<20))
	output := saveObjects(t, returned, &guest)
	if returned.lastID <= objectID(len(loaded.objectsByID)) {
		t.Fatal("new objects did not receive new IDs")
	}
	loadObjects(t, saved.decoder(ctx, output), &host)
	if host.A != b || host.B != a || a.Value != 12 || b.Value != 20 || a.Next != b || b.Next != a {
		t.Fatal("pointer swap, original identity or cycle was lost")
	}
	if host.Dropped != nil || dropped.Value != 31 {
		t.Fatal("unlinked object's mutation was lost")
	}
	if host.New == nil || host.New == guest.New || host.New.Next != a || host.New.Value != 40 {
		t.Fatal("new object was not relocated back to the host graph")
	}
	if host.Map != nil || host.MapAlias != nil || len(m) != 2 || m["delete"] != nil || m["keep"] != a || m["new"] != host.New {
		t.Fatal("map deletion, detached handle or map pointer relocation was lost")
	}
	if array != [3]int{1, 8, 3} || host.SliceAlias[1] != 8 || &host.SliceAlias[0] != &array[0] || len(host.Slice) != 4 || &host.Slice[0] == &array[0] {
		t.Fatal("old backing array or new slice allocation was lost")
	}
	if host.Field != &a.Value || host.Value.Addr().Interface().(*int64) != &a.Value {
		t.Fatal("interior pointer or reflect.Value identity was lost")
	}
}

func TestObjectIDRepeatedRoundTrip(t *testing.T) {
	n := &graphNode{Value: 10}
	host := [2]*graphNode{n, n}
	ctx := context.Background()
	saved := newEncodeState(ctx, make([]byte, 1<<20))
	input := saveObjects(t, saved, &host)
	var guest [2]*graphNode
	loaded := newDecodeState(ctx, input)
	loadObjects(t, loaded, &guest)
	for i := int64(11); i < 15; i++ {
		guest[0].Value = i
		returned := loaded.encoder(ctx, make([]byte, 1<<20))
		output := saveObjects(t, returned, &guest)
		if returned.lastID != saved.lastID {
			t.Fatal("unchanged graph acquired new IDs")
		}
		restored := saved.decoder(ctx, output)
		loadObjects(t, restored, &host)
		if host[0] != n || host[1] != n || n.Value != i {
			t.Fatal("original object was replaced")
		}
		saved = restored.encoder(ctx, make([]byte, 1<<20))
		input = saveObjects(t, saved, &host)
		loaded = returned.decoder(ctx, input)
		loadObjects(t, loaded, &guest)
	}
}

func TestObjectIDMergedStorage(t *testing.T) {
	n := &graphNode{Value: 10}
	n.Next = n
	other := &graphNode{Value: 20}
	// The parent absorbs two earlier field objects. One ID becomes a hole.
	host := [5]any{&n.Value, &n.Next, n, other, nil}
	ctx := context.Background()
	saved := newEncodeState(ctx, make([]byte, 1<<20))
	input := saveObjects(t, saved, &host)
	var guest [5]any
	loaded := newDecodeState(ctx, input)
	loadObjects(t, loaded, &guest)
	*guest[0].(*int64) = 11
	*guest[1].(**graphNode) = guest[3].(*graphNode)
	guest[4] = &graphNode{Value: 30, Next: guest[2].(*graphNode)}
	returned := loaded.encoder(ctx, make([]byte, 1<<20))
	output := saveObjects(t, returned, &guest)
	for id := range saved.pending {
		if returned.pending[id] == nil {
			t.Fatalf("existing ID %d was lost", id)
		}
	}
	if returned.lastID != saved.lastID+1 {
		t.Fatal("new object reused an old ID")
	}
	loadObjects(t, saved.decoder(ctx, output), &host)
	if host[0] != &n.Value || host[1] != &n.Next || host[2] != n || host[3] != other || n.Value != 11 || n.Next != other || host[4].(*graphNode).Next != n {
		t.Fatal("merged fields or parent lost their identity")
	}
}

func TestObjectIDRetainedStorage(t *testing.T) {
	array := [2]int{10, 20}
	host := struct {
		Field *int
		Whole *[2]int
	}{Field: &array[0]}
	ctx := context.Background()
	saved := newEncodeState(ctx, make([]byte, 1<<20))
	input := saveObjects(t, saved, &host)
	restored := saved.decoder(ctx, input)
	loadObjects(t, restored, &host)
	// A later save must not repurpose the field's existing ID for its
	// previously unseen parent. The other process allocated only an int.
	host.Whole = &array
	next := restored.encoder(ctx, make([]byte, 1<<20))
	err := safely(func() { next.Save(reflect.ValueOf(&host).Elem()) })
	if err == nil || !strings.Contains(err.Error(), "cannot change retained storage") {
		t.Fatalf("retained object ID was repurposed: %v", err)
	}
}

func TestLoadClearsExistingValues(t *testing.T) {
	type root struct {
		N        int
		B        bool
		S        string
		P        *int
		M        map[string]int
		A        []int
		I        any
		F        func()
		C        chan int
		Pointers [1]*int
		Slices   [1][]int
		Maps     [1]map[string]int
		Channels [1]chan int
	}
	n := 42
	var src root
	dst := root{42, true, "old", &n, map[string]int{"old": 1}, []int{1}, 42, func() {}, make(chan int), [1]*int{&n}, [1][]int{{1}}, [1]map[string]int{{"old": 1}}, [1]chan int{make(chan int)}}
	roundtrip(t, &src, &dst)
	if !reflect.ValueOf(dst).IsZero() {
		t.Fatalf("old values survived load: %#v", dst)
	}
}

func TestObjectIDChannelSnapshot(t *testing.T) {
	ctx := context.Background()
	ch := make(chan *graphNode, 3)
	n := &graphNode{Value: 1}
	ch <- n
	host := struct {
		Channel chan *graphNode
		Alias   <-chan *graphNode
		Node    *graphNode
	}{ch, ch, n}
	saved := newEncodeState(ctx, make([]byte, 1<<20))
	input := saveObjects(t, saved, &host)
	guest := reflect.New(reflect.TypeOf(host))
	loaded := newDecodeState(ctx, input)
	loadObjects(t, loaded, guest.Interface())
	guestChannel := guest.Elem().Field(0).Interface().(chan *graphNode)
	value := <-guestChannel
	value.Value = 2
	guestChannel <- value
	guestChannel <- value
	close(guestChannel)
	returned := loaded.encoder(ctx, make([]byte, 1<<20))
	output := saveObjects(t, returned, guest.Interface())
	loadObjects(t, saved.decoder(ctx, output), &host)
	if host.Channel == ch || host.Alias != host.Channel || len(ch) != 1 || len(host.Channel) != 2 || n.Value != 2 {
		t.Fatal("channel snapshot modified the original queue or lost aliases")
	}
	if <-host.Channel != n || <-host.Channel != n {
		t.Fatal("channel elements lost their object identity")
	}
	if _, ok := <-host.Channel; ok {
		t.Fatal("returned channel was not closed")
	}
	select {
	case ch <- n:
	default:
		t.Fatal("original channel no longer accepts sends")
	}
}
