package state

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func TestSubsliceRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name           string
		low, high, max int
	}{
		{"same-capacity", 0, 2, 6},
		{"suffix", 2, 4, 6},
		{"capped-prefix", 0, 2, 2},
		{"capped-suffix", 1, 3, 4},
	} {
		for _, partFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/partFirst=%v", test.name, partFirst), func(t *testing.T) {
				type storage struct {
					values [6]int
				}
				type root struct {
					Views   [2][]int
					Element *int
					Backing *storage
				}
				original := &storage{values: [6]int{10, 20, 30, 40, 50, 60}}
				host := root{
					Views:   [2][]int{original.values[:4], original.values[test.low:test.high:test.max]},
					Element: &original.values[test.low],
					Backing: original,
				}
				whole, part := 0, 1
				if partFirst {
					host.Views[0], host.Views[1] = host.Views[1], host.Views[0]
					whole, part = 1, 0
				}
				ctx := context.Background()
				mem := make([]byte, 1<<20)
				var hostState, guestState State
				n, _, err := hostState.Save(ctx, mem, &host)
				if err != nil {
					t.Fatal(err)
				}
				var guest root
				if _, err := guestState.Load(ctx, mem[:n], &guest); err != nil {
					t.Fatal(err)
				}
				if guest.Backing == original || !reflect.DeepEqual(guest, host) {
					t.Fatal("guest did not restore independent backing storage")
				}
				if &guest.Views[whole][0] != &guest.Backing.values[0] || &guest.Views[part][0] != &guest.Views[whole][test.low] || guest.Element != &guest.Views[part][0] {
					t.Fatal("guest lost the subarray or element alias")
				}
				if len(guest.Views[whole]) != 4 || cap(guest.Views[whole]) != 6 || len(guest.Views[part]) != test.high-test.low || cap(guest.Views[part]) != test.max-test.low {
					t.Fatal("guest changed slice length or capacity")
				}
				guest.Views[part][0] = 101
				guest.Views[part][:cap(guest.Views[part])][test.max-test.low-1] = 202
				if *guest.Element != 101 || guest.Backing.values[test.max-1] != 202 || original.values[test.low] == 101 {
					t.Fatal("guest writes lost their aliases or reached host memory")
				}
				n, _, err = guestState.Save(ctx, mem, &guest)
				if err != nil {
					t.Fatal(err)
				}
				if guestState.saved.lastID != hostState.saved.lastID {
					t.Fatal("subarray views allocated new object IDs on return")
				}
				if _, err := hostState.Load(ctx, mem[:n], &host); err != nil {
					t.Fatal(err)
				}
				if host.Backing != original || host.Element != &original.values[test.low] || &host.Views[whole][0] != &original.values[0] || &host.Views[part][0] != host.Element {
					t.Fatal("return replaced the host storage or lost an alias")
				}
				if !reflect.DeepEqual(host, guest) || original.values[test.low] != 101 || original.values[test.max-1] != 202 {
					t.Fatal("return did not write changes into the original storage")
				}
			})
		}
	}
}

func TestArrayRangeBounds(t *testing.T) {
	root := reflect.ValueOf(&[4]int{10, 20, 30, 40}).Elem()
	for _, span := range []arrayRange{
		{start: 5, length: 0},
		{start: 3, length: 2},
		{start: ^uintValue(0), length: 1},
		{start: 1, length: ^uintValue(0)},
	} {
		if err := safely(func() { walkChild([]dot{span}, root) }); err == nil {
			t.Fatalf("accepted out-of-bounds array range %+v", span)
		}
	}
	if err := safely(func() { walkChild([]dot{arrayRange{length: 1}}, root.Index(0)) }); err == nil {
		t.Fatal("accepted an array range on a scalar")
	}
	for _, test := range []struct {
		length int
		addr   uintptr
	}{
		{1, 0x1001},
		{1, 0x0ff8},
		{2, 0x1018},
	} {
		if err := safely(func() {
			traverse(root.Type(), reflect.ArrayOf(test.length, root.Type().Elem()), 0x1000, test.addr)
		}); err == nil {
			t.Fatalf("accepted array length=%d at %#x", test.length, test.addr)
		}
	}
}
