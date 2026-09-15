//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"fmt"
	"maps"
	"os"
	"reflect"
	"runtime"
	"strings"
)

const imageBytes = 16 << 20

const standardStreamBit = uintptr(1) << 62

// Sentry imports descriptors 0, 1 and 2. Rebind the corresponding standard Go
// objects; os.File's locks and poller state belong to each runtime separately.
func standardStream(v reflect.Value) uintptr {
	if v.Type() != reflect.TypeFor[*os.File]() {
		return 0
	}
	for i, f := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		if f != nil && v.Pointer() == reflect.ValueOf(f).Pointer() && f.Fd() == uintptr(i) {
			return uintptr(i + 1)
		}
	}
	return 0
}

func transferable(t reflect.Type) error {
	// These values contain scheduler, OS, or synchronization relationships that
	// copying their fields cannot recreate (e.g. a locked sync.Mutex).
	pkg, name := t.PkgPath(), t.Name()
	if t.Kind() == reflect.Struct && (pkg == "sync" || pkg == "sync/atomic" ||
		pkg == "internal/poll" || pkg == "os" && (name == "File" || name == "file" || name == "Process") ||
		pkg == "time" && (name == "Timer" || name == "Ticker") ||
		pkg == "context" && (name == "cancelCtx" || name == "timerCtx" || name == "afterFuncCtx")) {
		return fmt.Errorf("%s contains process-local state", t)
	}
	return nil
}

func snapshot(v reflect.Value) reflect.Value {
	v = addressable(v)
	c := reflect.New(v.Type()).Elem()
	c.Set(v)
	return c
}

func (w *imageWriter) retain(key objectRef, value reflect.Value) {
	if w.retained[key] {
		return
	}
	w.retained[key] = true
	w.anchors = append(w.anchors, snapshot(value))
}

func newImage(mem []byte, metadata *nativeMetadata, functions map[uintptr]nativeLayout) *valueImage {
	im := &valueImage{mem: mem, used: 4096, types: maps.Clone(metadata.types), typeIDs: make(map[reflect.Type]uintptr), native: functions, metadata: metadata}
	for id, typ := range im.types {
		im.typeIDs[typ] = id
	}
	return im
}

func (im *valueImage) inherit(from *valueImage) {
	im.types, im.typeIDs = maps.Clone(from.types), maps.Clone(from.typeIDs)
	im.typeDefs = append([]transferType(nil), from.typeDefs...)
	im.programs = append([]*ixgoProgram(nil), from.programs...)
}

// encode writes the root, followed by roots retaining every imported object.
// Retention preserves mutations such as `p.Value++; p = nil` for host aliases.
func (im *valueImage) encode(fn reflect.Value, retained []reflect.Value) (*imageWriter, error) {
	if err := im.discover(fn); err != nil {
		return nil, err
	}
	im.used = 4096
	clear(im.mem[:4096])
	w := &imageWriter{valueImage: im, refs: make(map[objectRef]uintptr), retained: make(map[objectRef]bool)}
	root, err := w.alloc(fn.Type().Size(), fn.Type().Align())
	if err != nil {
		return nil, err
	}
	if err := w.copy(snapshot(fn), root, "fn", 0); err != nil {
		return nil, err
	}
	programs, err := w.programValues()
	if err != nil {
		return nil, err
	}
	var slots []uintptr
	var types []reflect.Type
	for i := 0; ; i++ {
		var v reflect.Value
		if retained == nil {
			if i >= len(w.anchors) {
				break
			}
			v = w.anchors[i]
		} else {
			if i >= len(retained) {
				break
			}
			v = retained[i]
		}
		slot, err := w.alloc(v.Type().Size(), v.Type().Align())
		if err != nil {
			return nil, err
		}
		if err := w.copy(snapshot(v), slot, fmt.Sprintf("retained[%d]", i), 0); err != nil {
			return nil, err
		}
		slots = append(slots, slot)
		types = append(types, v.Type())
	}
	table, err := w.alloc(uintptr(len(slots))*16, 8)
	if err != nil {
		return nil, err
	}
	for i, slot := range slots {
		id, err := w.typeID(types[i])
		if err != nil {
			return nil, err
		}
		w.put(table+uintptr(i)*16, id)
		w.put(table+uintptr(i)*16+8, slot)
	}
	functions, err := w.alloc(uintptr(len(im.native))*16, 8)
	if err != nil {
		return nil, err
	}
	i := uintptr(0)
	for pc, layout := range im.native {
		w.put(functions+i*16, pc)
		id, err := w.typeID(layout.signature)
		if err != nil {
			return nil, err
		}
		w.put(functions+i*16+8, id)
		i++
	}
	if err := w.metadataDescription(programs); err != nil {
		return nil, err
	}
	w.put(0, root)
	w.put(8, im.used)
	w.put(16, table)
	w.put(24, uintptr(len(slots)))
	w.put(32, functions)
	w.put(40, uintptr(len(im.native)))
	runtime.KeepAlive(fn)
	return w, nil
}

func (im *valueImage) header() error {
	if len(im.mem) < 4096 {
		return fmt.Errorf("truncated image header")
	}
	// Header words are outside the payload range checked by word().
	im.used = headerWord(im.mem, 8)
	if im.used < 4096 || im.used > uintptr(len(im.mem)) {
		return fmt.Errorf("invalid image length %d", im.used)
	}
	return nil
}

func (im *valueImage) authorizeFunctions() error {
	table, count := headerWord(im.mem, 32), headerWord(im.mem, 40)
	if count > im.used/16 {
		return fmt.Errorf("invalid function count")
	}
	if _, err := im.bytes(table, count*16); err != nil {
		return err
	}
	for i := uintptr(0); i < count; i++ {
		pc, _ := im.word(table + i*16)
		typ, _ := im.word(table + i*16 + 8)
		t, ok := im.types[typ]
		if !ok || t.Kind() != reflect.Func {
			return fmt.Errorf("invalid function type %#x", typ)
		}
		layout, err := im.metadata.layout(pc, t)
		if err != nil {
			return err
		}
		if old, ok := im.native[pc]; ok && old.signature != t {
			return fmt.Errorf("conflicting function signatures")
		}
		im.native[pc] = layout
	}
	return nil
}

func (im *valueImage) decode(bindings map[objectRef]reflect.Value) (reflect.Value, []reflect.Value, error) {
	if err := im.header(); err != nil {
		return reflect.Value{}, nil, err
	}
	refs := maps.Clone(im.initial)
	if refs == nil {
		refs = make(map[objectRef]reflect.Value)
	}
	r := &imageReader{valueImage: im, refs: refs, bindings: bindings}
	fn := reflect.New(reflect.TypeFor[func()]()).Elem()
	if err := r.copy(fn, headerWord(im.mem, 0), "fn", 0); err != nil {
		return reflect.Value{}, nil, err
	}
	if fn.IsNil() {
		return reflect.Value{}, nil, fmt.Errorf("nil image function")
	}
	table, count := headerWord(im.mem, 16), headerWord(im.mem, 24)
	if count > im.used/16 {
		return reflect.Value{}, nil, fmt.Errorf("invalid retained object count")
	}
	if _, err := im.bytes(table, count*16); err != nil {
		return reflect.Value{}, nil, err
	}
	values := make([]reflect.Value, int(count))
	for i := range values {
		typ, _ := im.word(table + uintptr(i)*16)
		slot, _ := im.word(table + uintptr(i)*16 + 8)
		t, ok := im.types[typ]
		if !ok {
			return reflect.Value{}, nil, fmt.Errorf("unknown retained type %#x", typ)
		}
		values[i] = reflect.New(t).Elem()
		if err := r.copy(values[i], slot, fmt.Sprintf("retained[%d]", i), 0); err != nil {
			return reflect.Value{}, nil, err
		}
	}
	return fn, values, nil
}

func (im *valueImage) bindingKey(t reflect.Type, slot uintptr) (objectRef, error) {
	addr, err := im.word(slot)
	if err != nil {
		return objectRef{}, err
	}
	key := objectRef{typ: t, addr: addr}
	switch t.Kind() {
	case reflect.Pointer:
		key.typ = t.Elem()
	case reflect.Map:
		key.span = -1
	case reflect.Func:
		key.span = -2
	case reflect.Slice:
		cap, err := im.word(slot + 16)
		if err != nil {
			return key, err
		}
		key.span = int(cap) + 1
	default:
		return key, fmt.Errorf("invalid retained kind %s", t.Kind())
	}
	if addr == 0 {
		return key, fmt.Errorf("retained object is nil")
	}
	return key, nil
}

func objectAddress(v reflect.Value) uintptr {
	if v.Kind() == reflect.Func {
		return functionAddress(v)
	}
	if v.Kind() == reflect.Map {
		return uintptr(v.UnsafePointer())
	}
	return v.Pointer()
}

func (im *valueImage) commit(original []reflect.Value) error {
	// Decode against a private copy first. Malformed output cannot partially
	// change host values, or race validation through the shared mapping.
	_, values, err := im.decode(nil)
	if err != nil {
		return err
	}
	if len(values) != len(original) {
		return fmt.Errorf("retained object count changed")
	}
	table := headerWord(im.mem, 16)
	bindings := make(map[objectRef]reflect.Value)
	for i, a := range original {
		v := values[i]
		if v.Type() != a.Type() || v.IsNil() {
			return fmt.Errorf("retained[%d] changed type or became nil", i)
		}
		if v.Kind() == reflect.Slice && v.Cap() != a.Cap() {
			return fmt.Errorf("retained[%d] changed backing capacity", i)
		}
		if v.Kind() == reflect.Func && v.Pointer() != a.Pointer() {
			return fmt.Errorf("retained[%d] changed code entry", i)
		}
		if v.Kind() == reflect.Func {
			before, err := im.metadata.ixgoCallback(a)
			if err != nil {
				return err
			}
			after, err := im.metadata.ixgoCallback(v)
			if err != nil {
				return err
			}
			if before != nil && (after == nil || before.interp != after.interp || before.pfn.Fn != after.pfn.Fn) {
				return fmt.Errorf("retained[%d] changed ixgo function", i)
			}
		}
		slot, _ := im.word(table + uintptr(i)*16 + 8)
		key, err := im.bindingKey(v.Type(), slot)
		if err != nil {
			return err
		}
		if old, ok := bindings[key]; ok && objectAddress(old) != objectAddress(a) {
			return fmt.Errorf("retained objects were merged")
		}
		bindings[key] = a
	}
	_, _, err = im.decode(bindings)
	runtime.KeepAlive(original)
	return err
}

func validRseqSetting(value string) bool {
	for _, setting := range strings.Split(value, ":") {
		if strings.HasPrefix(setting, "glibc.pthread.rseq=") {
			return setting == "glibc.pthread.rseq=0"
		}
	}
	return false
}
