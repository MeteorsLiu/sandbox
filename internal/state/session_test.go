package state

import (
	"context"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	type node struct {
		N int
		P *node
	}
	a, b := &node{N: 1}, &node{N: 2}
	a.P, b.P = b, a
	host := [3]*node{a, b, a}
	var guest [3]*node
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
		if guest[0] == host[0] || guest[0] != guest[2] || guest[0].P != guest[1] {
			t.Fatal("copied objects lost isolation or aliases")
		}
		guest[0].N++
		n, _, err = destination.Save(ctx, mem, &guest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.Load(ctx, mem[:n], &host); err != nil {
			t.Fatal(err)
		}
		if host[0] != a || host[1] != b || a.N != i+2 || a.P != b || b.P != a {
			t.Fatal("original objects were replaced or not updated")
		}
	}
}

func TestStateRejectedReturnDoesNotWriteBack(t *testing.T) {
	a, b := 1, 2
	host := [2]*int{&a, &b}
	var guest [2]*int
	var source, destination State
	ctx := context.Background()
	mem := make([]byte, 1<<20)
	n, _, err := source.Save(ctx, mem, &host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := destination.Load(ctx, mem[:n], &guest); err != nil {
		t.Fatal(err)
	}
	*guest[0], *guest[1] = 10, 20
	n, _, err = destination.Save(ctx, mem, &guest)
	if err != nil {
		t.Fatal(err)
	}
	for cut := 1; cut <= 8; cut++ {
		if _, err := source.Load(ctx, mem[:n-cut], &host); err == nil {
			t.Fatal("truncated return accepted")
		}
		if a != 1 || b != 2 || host[0] != &a || host[1] != &b {
			t.Fatal("failed load partially updated the source graph")
		}
	}
	if _, err := source.Load(ctx, mem[:n], &host); err != nil {
		t.Fatal(err)
	}
	if a != 10 || b != 20 {
		t.Fatal("valid return after rejected data lost object identities")
	}
}

func TestStateConcurrentSharedMapLoad(t *testing.T) {
	const workers, entries = 8, 1024
	shared := make(map[int]int, entries)
	for i := 0; i < entries; i++ {
		shared[i] = 0
	}
	var hosts [workers]State
	var roots [workers]map[int]int
	var images [workers][]byte
	ctx := context.Background()
	for i := range hosts {
		roots[i] = shared
		mem := make([]byte, 1<<20)
		n, _, err := hosts[i].Save(ctx, mem, &roots[i])
		if err != nil {
			t.Fatal(err)
		}
		var guest State
		var returned map[int]int
		if _, err := guest.Load(ctx, mem[:n], &returned); err != nil {
			t.Fatal(err)
		}
		for key := range returned {
			returned[key] = i + 1
		}
		n, _, err = guest.Save(ctx, mem, &returned)
		if err != nil {
			t.Fatal(err)
		}
		images[i] = mem[:n]
	}
	start := make(chan struct{})
	errors := make(chan error, workers)
	for i := range hosts {
		go func() {
			<-start
			_, err := hosts[i].Load(ctx, images[i], &roots[i])
			errors <- err
		}()
	}
	close(start)
	for range hosts {
		if err := <-errors; err != nil {
			t.Error(err)
		}
	}
	// The last complete snapshot wins; entry locking does not merge changes
	// made by different guests or prescribe which guest writes back last.
	want := shared[0]
	if want < 1 || want > workers || len(shared) != entries {
		t.Fatalf("invalid restored map: first=%d len=%d", want, len(shared))
	}
	for key := 0; key < entries; key++ {
		if got, ok := shared[key]; !ok || got != want {
			t.Fatalf("interleaved snapshots at key %d: got %d (present=%v), want %d", key, got, ok, want)
		}
	}
	shared[entries] = want
	for i, root := range roots {
		if root[entries] != want {
			t.Fatalf("state %d replaced the shared map", i)
		}
	}
}
