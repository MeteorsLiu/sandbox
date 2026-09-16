package state

import (
	"reflect"
	"runtime"
	"unsafe"
)

// Prefix of runtime.hchan in Go 1.26.6. The lock itself remains owned by
// runtime; its size depends on GOEXPERIMENT=staticlockranking.
type runtimeChannel struct {
	qcount, dataqsiz uint
	buf              unsafe.Pointer
	elemsize         uint16
	closed           uint32
	timer, elemtype  unsafe.Pointer
	sendx, recvx     uint
	recvq, sendq     [2]unsafe.Pointer
	bubble           unsafe.Pointer
	lock             [0]uintptr
}

//go:linkname channelLock runtime.lock
func channelLock(unsafe.Pointer)

//go:linkname channelUnlock runtime.unlock
func channelUnlock(unsafe.Pointer)

//go:linkname channelBuffer runtime.chanbuf
func channelBuffer(*runtimeChannel, uint) unsafe.Pointer

//go:linkname channelCopy runtime.typedmemmove
func channelCopy(typ, dst, src unsafe.Pointer)

func channelHeader(value reflect.Value) *runtimeChannel {
	if runtime.Version() != "go1.26.6" {
		Failf("channel snapshots require go1.26.6, got %s", runtime.Version())
	}
	return (*runtimeChannel)(value.UnsafePointer())
}

// The caller supplies typed GC-visible storage before taking the runtime lock.
// Do not allocate, grow the stack, or run reflection while the lock is held.
//
//go:nosplit
//go:norace
func copyChannelQueue(c *runtimeChannel, dst unsafe.Pointer, count uint) (closed bool, reason int) {
	channelLock(unsafe.Pointer(&c.lock))
	switch {
	case c.recvq[0] != nil || c.sendq[0] != nil:
		reason = 1
	case c.timer != nil:
		reason = 2
	case c.bubble != nil:
		reason = 3
	case c.qcount != count:
		reason = 4
	default:
		closed = c.closed != 0
		index := c.recvx
		for i := uint(0); i < count; i++ {
			channelCopy(c.elemtype, unsafe.Add(dst, uintptr(i)*uintptr(c.elemsize)), channelBuffer(c, index))
			index++
			if index == c.dataqsiz {
				index = 0
			}
		}
	}
	channelUnlock(unsafe.Pointer(&c.lock))
	return
}

func (es *encodeState) encodeChannel(obj reflect.Value, dest *object) {
	c := channelHeader(obj)
	count := obj.Len()
	values := reflect.MakeSlice(reflect.SliceOf(obj.Type().Elem()), count, count)
	closed, reason := copyChannelQueue(c, values.UnsafePointer(), uint(values.Len()))
	runtime.KeepAlive(obj)
	switch reason {
	case 1:
		Failf("cannot snapshot channel with waiting goroutines")
	case 2:
		Failf("cannot snapshot timer-backed channel")
	case 3:
		Failf("cannot snapshot synctest channel")
	case 4:
		Failf("channel changed during snapshot")
	}
	data := &channelData{Closed: boolValue(closed), Values: arrayValue{Contents: make([]object, values.Len())}}
	*dest = data
	for i := 0; i < values.Len(); i++ {
		es.encodeObject(values.Index(i), encodeAsValue, &data.Values.Contents[i])
	}
}

func (ds *decodeState) decodeChannelRef(obj reflect.Value, encoded *channelValue) {
	if encoded.Ref.Root == 0 {
		return
	}
	if obj.Kind() != reflect.Chan || len(encoded.Ref.Dots) != 0 || uint64(encoded.Capacity) > uint64(^uint(0)>>1) {
		Failf("invalid channel reference")
	}
	id := objectID(encoded.Ref.Root)
	ds.growObjectsByID(id)
	ods := ds.objectsByID[id-1]
	if ods == nil {
		typ := reflect.ChanOf(reflect.BothDir, obj.Type().Elem())
		value := reflect.New(typ).Elem()
		value.Set(reflect.MakeChan(typ, int(encoded.Capacity)))
		ods = ds.addObject(id, value)
		if data, ok := ds.deferred[id]; ok {
			delete(ds.deferred, id)
			ds.decodeObject(ods, value, data)
		}
	}
	if ods.obj.Kind() != reflect.Chan || ods.obj.Type().Elem() != obj.Type().Elem() || ods.obj.Cap() != int(encoded.Capacity) {
		Failf("inconsistent channel alias")
	}
	obj.Set(ods.obj.Convert(obj.Type()))
}

func (ds *decodeState) decodeChannel(ods *objectDecodeState, obj reflect.Value, encoded *channelData) {
	if obj.Kind() != reflect.Chan || obj.IsNil() || len(encoded.Values.Contents) > obj.Cap() {
		Failf("invalid channel contents")
	}
	c := channelHeader(obj)
	typ := obj.Type().Elem()
	zero := reflect.Zero(typ)
	for range encoded.Values.Contents {
		if !obj.TrySend(zero) {
			Failf("channel contents exceed available capacity")
		}
	}
	// Decode in place: deferred pointer targets and StateLoad callbacks must
	// update the actual queue slots, not temporary values already sent by copy.
	// As with struct fields, reference cycles impose no implicit LoadWait.
	for i, value := range encoded.Values.Contents {
		slot := reflect.NewAt(typ, channelBuffer(c, uint(i))).Elem()
		ds.decodeObject(ods, slot, value)
	}
	if encoded.Closed {
		obj.Close()
	}
}
