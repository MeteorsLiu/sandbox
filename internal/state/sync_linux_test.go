//go:build linux && (amd64 || arm64)

package state

import (
	"context"
	"sync"
	"testing"
)

func TestSyncPoolNewRoundTrip(t *testing.T) {
	type root struct {
		Pool  sync.Pool
		Alias *sync.Pool
		Calls int
	}
	host := new(root)
	host.Alias = &host.Pool
	host.Pool.New = func() any {
		host.Calls++
		return host
	}
	host.Pool.Put("host cache")
	ctx := context.Background()
	saved := newEncodeState(ctx, make([]byte, 1<<20))
	input := saveObjects(t, saved, host)
	var guest root
	loaded := newDecodeState(ctx, input)
	loadObjects(t, loaded, &guest)
	if guest.Alias != &guest.Pool || guest.Pool.New == nil || guest.Calls != 0 || host.Calls != 0 {
		t.Fatal("Pool lost its alias or New, or invoked New while transferring")
	}
	if got := guest.Pool.Get(); got != &guest || guest.Calls != 1 || host.Calls != 0 {
		t.Fatal("Pool retained cached entries or New lost its relocated captures")
	}
	guest.Pool.Put("guest cache")
	host.Pool.Put("old destination cache")
	returned := loaded.encoder(ctx, make([]byte, 1<<20))
	output := saveObjects(t, returned, &guest)
	loadObjects(t, saved.decoder(ctx, output), host)
	if host.Alias != &host.Pool || host.Pool.New == nil || host.Calls != 1 {
		t.Fatal("returned Pool lost its alias, New or captured mutation")
	}
	if got := host.Pool.Get(); got != host || host.Calls != 2 || guest.Calls != 1 {
		t.Fatal("returned Pool retained cached entries or New lost its host captures")
	}
}
