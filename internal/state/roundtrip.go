package state

import (
	"context"
	"io"
	"reflect"
	"sync"

	"github.com/xgo-dev/sandbox/internal/reflectxtype"
)

// Separate States can retain the same host objects. Serialize their restores,
// including validation and hooks, so two Loads cannot write the same map.
var loadMu sync.Mutex

// State retains object identities across alternating Save and Load calls.
// The zero value is ready for use. Each participant owns its own State and
// keeps it alive until the round trip finishes, including unlinked objects.
// Calls on one State must not overlap, and the root's storage must not change.
type State struct {
	saved  *savedState
	loaded *loadedState
}

// Only object identity survives a transfer, not its encoded value or the
// traversal bookkeeping. The slice index is objectID-1; holes stay empty.
type objectState struct {
	obj reflect.Value
	how encodeStrategy
}

type savedState struct {
	objectsByID      []objectState
	native           nativeState
	reflectxSnapshot *reflectxtype.Snapshot
}

type loadedState struct {
	objectsByID  []objectState
	native       nativeState
	reflectx     *reflectxtype.ReflectType
	makeFuncs    map[reflect.Value]reflect.Value
	methodValues map[reflect.Value]reflect.Value
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
		s.saved, s.loaded = es.saved(), nil
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
	loadMu.Lock()
	defer loadMu.Unlock()

	ds := newDecodeState(ctx, mem)
	err := safely(func() {
		if s.saved != nil {
			check := newDecodeState(ctx, mem)
			check.native = s.saved.native
			check.reflectxSnapshot = s.saved.reflectxSnapshot
			for i, saved := range s.saved.objectsByID {
				if !saved.obj.IsValid() {
					continue
				}
				value := reflect.New(saved.obj.Type()).Elem()
				if saved.how == encodeMapAsValue {
					value.Set(reflect.MakeMap(value.Type()))
				} else if saved.how == encodeChannelAsValue {
					value.Set(reflect.MakeChan(value.Type(), saved.obj.Cap()))
				}
				check.addObject(objectID(i+1), value).how = saved.how
			}
			check.Load(check.lookup(1).obj)
			ds = s.saved.decoder(ctx, mem)
		}
		ds.Load(reflect.ValueOf(rootPtr).Elem())
	})
	if err == nil {
		s.loaded, s.saved = ds.loaded(), nil
	}
	return ds.stats, err
}

func (es *encodeState) saved() *savedState {
	s := &savedState{
		objectsByID: make([]objectState, es.lastID),
		native:      es.native,
	}
	for id, object := range es.pending {
		s.objectsByID[id-1] = objectState{obj: object.obj, how: object.how}
	}
	if snapshot := es.reflectxSnapshot; snapshot != nil {
		// Method functions have already entered the object graph. Open only
		// needs the definitions and original types to validate a returned table.
		s.reflectxSnapshot = &reflectxtype.Snapshot{Data: snapshot.Data, IDs: snapshot.IDs}
	}
	return s
}

func (ds *decodeState) loaded() *loadedState {
	s := &loadedState{
		objectsByID:  make([]objectState, len(ds.objectsByID)),
		native:       ds.native,
		reflectx:     ds.reflectx,
		makeFuncs:    ds.makeFuncs,
		methodValues: ds.methodValues,
	}
	for i, object := range ds.objectsByID {
		if object != nil {
			s.objectsByID[i] = objectState{obj: object.obj, how: object.how}
		}
	}
	return s
}

// decoder reuses the objects from this save when loading its returned graph.
// Keeping s alive retains even objects the guest later unlinks from the root.
// No source address is written to the stream; both sides use the existing IDs.
func (s *savedState) decoder(ctx context.Context, mem []byte) *decodeState {
	ds := newDecodeState(ctx, mem)
	ds.native = s.native
	ds.reflectxSnapshot = s.reflectxSnapshot
	for i, saved := range s.objectsByID {
		value := saved.obj
		if !value.IsValid() {
			continue
		}
		if saved.how == encodeChannelAsValue {
			// Channels retain the independent-snapshot semantics of Load.
			// Never enqueue data into, or close, the original host channel.
			typ := reflect.ChanOf(reflect.BothDir, value.Type().Elem())
			channel := reflect.New(typ).Elem()
			channel.Set(reflect.MakeChan(typ, value.Cap()))
			value = channel
		}
		ods := ds.addObject(objectID(i+1), value)
		ods.how = saved.how
	}
	return ds
}

// encoder starts the return save with the decoded objects' existing IDs.
// For example, swapping two pointers changes their refs, not their object IDs.
func (s *loadedState) encoder(ctx context.Context, mem []byte) *encodeState {
	es := newEncodeState(ctx, mem)
	es.native = s.native
	es.reflectx = s.reflectx
	es.lastID = objectID(len(s.objectsByID))
	for i, decoded := range s.objectsByID {
		if !decoded.obj.IsValid() {
			continue
		}
		value := decoded.obj
		addr, size := value.Addr().Pointer(), value.Type().Size()
		if decoded.how == encodeMapAsValue || decoded.how == encodeChannelAsValue {
			addr, size = value.Pointer(), 1
		}
		oes := &objectEncodeState{id: objectID(i + 1), obj: value, how: decoded.how, retained: true}
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
	for callback, fn := range s.makeFuncs {
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
	for env, fn := range s.methodValues {
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
