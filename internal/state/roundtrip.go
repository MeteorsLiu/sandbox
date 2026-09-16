package state

import (
	"context"
	"reflect"
)

// decoder reuses the objects from this save when loading its returned graph.
// Keeping es alive retains even objects the guest later unlinks from the root.
// No source address is written to the stream; both sides use the existing IDs.
func (es *encodeState) decoder(ctx context.Context, mem []byte) *decodeState {
	ds := newDecodeState(ctx, mem)
	ds.native = es.native
	for id, saved := range es.pending {
		value := saved.obj
		if saved.how == encodeChannelAsValue {
			// Channels retain the independent-snapshot semantics of Load.
			// Never enqueue data into, or close, the original host channel.
			typ := reflect.ChanOf(reflect.BothDir, value.Type().Elem())
			channel := reflect.New(typ).Elem()
			channel.Set(reflect.MakeChan(typ, value.Cap()))
			value = channel
		}
		ods := ds.addObject(id, value)
		ods.how = saved.how
	}
	return ds
}

// encoder starts the return save with the decoded objects' existing IDs.
// For example, swapping two pointers changes their refs, not their object IDs.
func (ds *decodeState) encoder(ctx context.Context, mem []byte) *encodeState {
	es := newEncodeState(ctx, mem)
	es.native = ds.native
	es.lastID = objectID(len(ds.objectsByID))
	for _, decoded := range ds.objectsByID {
		if decoded == nil {
			continue
		}
		value := decoded.obj
		addr, size := value.Addr().Pointer(), value.Type().Size()
		if decoded.how == encodeMapAsValue || decoded.how == encodeChannelAsValue {
			addr, size = value.Pointer(), 1
		}
		oes := &objectEncodeState{id: decoded.id, obj: value, how: decoded.how, retained: true}
		es.pending[oes.id] = oes
		es.deferred.PushBack(oes)
		if size == 0 && addr == dummyAddr {
			es.zeroValues[value.Type()] = oes
			continue
		}
		_, gap := es.values.Find(addr)
		es.values.Insert(gap, addrRange{addr, addr + max(size, 1)}, oes)
	}
	// MakeFunc rebuilds a wrapper whose callback slot differs from the
	// decoded slot. Both locations must resolve to that same object ID.
	for callback, fn := range ds.makeFuncs {
		seg, _ := es.values.Find(callback.Addr().Pointer())
		if !seg.Ok() {
			Failf("MakeFunc callback object is missing")
		}
		actual := makeFuncCallback(fn)
		addr := actual.Addr().Pointer()
		_, gap := es.values.Find(addr)
		es.values.Insert(gap, addrRange{addr, addr + actual.Type().Size()}, seg.Value())
	}
	return es
}
