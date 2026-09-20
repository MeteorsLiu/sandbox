package state

import (
	"context"
	"io"
	"reflect"
)

// State retains object identities across alternating Save and Load calls.
// The zero value is ready for use. Each participant owns its own State and
// keeps it alive until the round trip finishes, including unlinked objects.
// Calls on one State must not overlap, and the root's storage must not change.
type State struct {
	saved  *encodeState
	loaded *decodeState
}

// Save writes the graph, preserving IDs from the preceding Load when present.
func (s *State) Save(ctx context.Context, mem []byte, rootPtr any) (int, Stats, error) {
	return s.save(ctx, mem, nil, rootPtr)
}

// SaveTo writes the same graph records as Save to an output stream.
// The caller owns flushing and closing the stream.
func (s *State) SaveTo(ctx context.Context, out io.Writer, rootPtr any) (int, Stats, error) {
	return s.save(ctx, nil, out, rootPtr)
}

func (s *State) save(ctx context.Context, mem []byte, out io.Writer, rootPtr any) (int, Stats, error) {
	es := newEncodeState(ctx, mem)
	err := safely(func() {
		if s.loaded != nil {
			es = s.loaded.encoder(ctx, mem)
		}
		es.w.out = out
		es.Save(reflect.ValueOf(rootPtr).Elem())
	})
	if err == nil {
		s.saved, s.loaded = es, nil
	}
	return es.w.pos, es.stats, err
}

// Load restores the graph into objects retained by the preceding Save.
// Before overwriting those objects, it decodes into independent storage so
// structural errors in returned records are detected before writeback.
// Custom StateLoad/AfterLoad hooks run in both passes and must be deterministic
// and confined to their decoded graph. Their external side effects cannot be
// rolled back, and a hook that fails only during writeback can partially apply.
func (s *State) Load(ctx context.Context, mem []byte, rootPtr any) (Stats, error) {
	ds := newDecodeState(ctx, mem)
	err := safely(func() {
		if s.saved != nil {
			check := newDecodeState(ctx, mem)
			check.native = s.saved.native
			check.reflectxSnapshot = s.saved.reflectxSnapshot
			for id, saved := range s.saved.pending {
				value := reflect.New(saved.obj.Type()).Elem()
				if saved.how == encodeMapAsValue {
					value.Set(reflect.MakeMap(value.Type()))
				} else if saved.how == encodeChannelAsValue {
					value.Set(reflect.MakeChan(value.Type(), saved.obj.Cap()))
				}
				check.addObject(id, value).how = saved.how
			}
			check.Load(check.lookup(1).obj)
			ds = s.saved.decoder(ctx, mem)
		}
		ds.Load(reflect.ValueOf(rootPtr).Elem())
	})
	if err == nil {
		s.loaded, s.saved = ds, nil
	}
	return ds.stats, err
}

// decoder reuses the objects from this save when loading its returned graph.
// Keeping es alive retains even objects the guest later unlinks from the root.
// No source address is written to the stream; both sides use the existing IDs.
func (es *encodeState) decoder(ctx context.Context, mem []byte) *decodeState {
	ds := newDecodeState(ctx, mem)
	ds.native = es.native
	ds.reflectxSnapshot = es.reflectxSnapshot
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
	es.reflectx = ds.reflectx
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
	// Rebuilt method wrappers keep the decoded environment's object ID, just
	// like MakeFunc callback slots above.
	for env, fn := range ds.methodValues {
		seg, _ := es.values.Find(env.Addr().Pointer())
		if !seg.Ok() {
			Failf("method environment object is missing")
		}
		actual := reflect.ValueOf(&methodValueStorage(fn).env).Elem()
		addr := actual.Addr().Pointer()
		_, gap := es.values.Find(addr)
		es.values.Insert(gap, addrRange{addr, addr + actual.Type().Size()}, seg.Value())
	}
	return es
}
