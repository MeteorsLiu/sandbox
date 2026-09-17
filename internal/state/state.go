// Copyright 2018 The gVisor Authors.
// Modified for sandbox: Save and Load operate on caller-owned byte slices.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package state provides functionality related to saving and loading object
// graphs.  For most types, it provides a set of default saving / loading logic
// that will be invoked automatically if custom logic is not defined.
//
//	Kind             Support
//	----             -------
//	Bool             default
//	Int              default
//	Int8             default
//	Int16            default
//	Int32            default
//	Int64            default
//	Uint             default
//	Uint8            default
//	Uint16           default
//	Uint32           default
//	Uint64           default
//	Float32          default
//	Float64          default
//	Complex64        default
//	Complex128       default
//	Array            default
//	Chan             independent queue snapshot (Go 1.26.6)
//	Func             native PC/ELF capture layout and MakeFunc (Linux amd64/arm64)
//	Interface        default
//	Map              default
//	Ptr              default
//	Slice            default
//	String           default
//	Struct           fields by default, or custom StateSave/StateLoad
//	UnsafePointer    custom
//
// Adapted from gVisor pkg/state at d1e35511e5a4. Object graph traversal is
// retained; memory.go provides the byte-slice transport.
package state

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
)

// objectID is a unique identifier assigned to each object to be serialized.
// Each instance of an object is considered separately, i.e. if there are two
// objects of the same type in the object graph being serialized, they'll be
// assigned unique objectIDs.
type objectID uint32

// typeID is the identifier for a type. Types are serialized and tracked
// alongside objects in order to avoid the overhead of encoding field names in
// all objects.
type typeID uint32

// ErrState is returned when an error is encountered during encode/decode.
type ErrState struct {
	// err is the underlying error.
	err error

	// trace is the stack trace.
	trace string
}

// Error returns a sensible description of the state error.
func (e *ErrState) Error() string {
	return fmt.Sprintf("%v:\n%s", e.err, e.trace)
}

// Unwrap implements standard unwrapping.
func (e *ErrState) Unwrap() error {
	return e.err
}

// Save writes the object graph to mem and returns the number of bytes written.
// The caller owns mem; Save does not allocate a replacement when it is full.
// The stream starts with standard reflect and reflectx type tables, followed
// by the object graph. Type references prefer the standard table. Dynamic
// reflectx types and method definitions are reconstructed in the second table.
// Method callbacks use the same closure graph as ordinary function values;
// local method entries are installed before interface values are restored.
// reflect.Type and xtype.Type values refer to these tables; host descriptor
// addresses are not transferred. Finish type construction and context resets
// before Save or Load, including constructors outside the source object graph.
// Channels preserve capacity, FIFO values, closed state and aliases in the new
// graph. Waiting goroutines, runtime-attached timers and synctest channels are
// rejected. Legacy timer modes without a channel timer link cannot be detected.
// The caller must keep the source graph quiescent throughout Save.
// Atomic wrappers preserve their values; atomic.Pointer[T] targets are relocated
// with the object graph. sync.Map saves entries through Range and rebuilds them
// through Store; keys and values share the same object graph. Its internal hash
// trie and synchronization state are not copied. Other sync structs are not traversed:
// they become zero values, including Once's done flag, WaitGroup's counter,
// Cond.L and Pool.New. Their waiters and synchronization state are not copied.
// reflect.Value preserves its represented type, value and addressability.
// Values obtained through unexported fields are rejected; their access flags
// are not transferred. The represented value must itself be supported by Save.
// Native functions require the same non-PIE Go executable with ELF symbols on
// both ends. Function values in the static descriptor range have no captures
// and save only their PC with an empty environment reference. Load creates a
// PC-only function value and caches its layout for subsequent saves. Closure
// allocation instructions identify captured environment types.
// Bound methods resolve their named receiver from the function symbol and
// static type links, then save the receiver as the method value's environment.
// Missing layouts are rejected. Captured objects are saved; globals are not.
// MakeFunc wrappers are rebuilt from the function type and saved callback;
// the callback and everything it captures must also be supported by Save.
// Standard stream references rebind to the local os.Stdin/Stdout/Stderr.
// Other files, OS processes, timers and cancellation contexts are rejected.
func Save(ctx context.Context, mem []byte, rootPtr any) (int, Stats, error) {
	var s State
	return s.Save(ctx, mem, rootPtr)
}

func newEncodeState(ctx context.Context, mem []byte) *encodeState {
	return &encodeState{
		ctx:            ctx,
		w:              writer{mem: mem},
		types:          makeTypeEncodeDatabase(),
		zeroValues:     make(map[reflect.Type]*objectEncodeState),
		pending:        make(map[objectID]*objectEncodeState),
		encodedStructs: make(map[reflect.Value]*structValue),
	}
}

// Load restores an object graph from the bytes written by Save.
func Load(ctx context.Context, mem []byte, rootPtr any) (Stats, error) {
	var s State
	return s.Load(ctx, mem, rootPtr)
}

func newDecodeState(ctx context.Context, mem []byte) *decodeState {
	return &decodeState{
		ctx:      ctx,
		r:        reader{mem: mem},
		types:    makeTypeDecodeDatabase(),
		deferred: make(map[objectID]object),
	}
}

// Sink is used for Type.StateSave.
type Sink struct {
	internal objectEncoder
}

// Save adds the given object to the map.
//
// You should pass always pointers to the object you are saving. For example:
//
//	type X struct {
//		A int
//		B *int
//	}
//
//	func (x *X) StateTypeInfo(m Sink) state.TypeInfo {
//		return state.TypeInfo{
//			Name:   "pkg.X",
//			Fields: []string{
//				"A",
//				"B",
//			},
//		}
//	}
//
//	func (x *X) StateSave(m Sink) {
//		m.Save(0, &x.A) // Field is A.
//		m.Save(1, &x.B) // Field is B.
//	}
//
//	func (x *X) StateLoad(m Source) {
//		m.Load(0, &x.A) // Field is A.
//		m.Load(1, &x.B) // Field is B.
//	}
func (s Sink) Save(slot int, objPtr any) {
	s.internal.save(slot, reflect.ValueOf(objPtr).Elem())
}

// SaveValue adds the given object value to the map.
//
// This should be used for values where pointers are not available, or casts
// are required during Save/Load.
//
// For example, if we want to cast external package type P.Foo to int64:
//
//	func (x *X) StateSave(m Sink) {
//		m.SaveValue(0, "A", int64(x.A))
//	}
//
//	func (x *X) StateLoad(m Source) {
//		m.LoadValue(0, new(int64), func(x any) {
//			x.A = P.Foo(x.(int64))
//		})
//	}
func (s Sink) SaveValue(slot int, obj any) {
	s.internal.save(slot, reflect.ValueOf(obj))
}

// Context returns the context object provided at save time.
func (s Sink) Context() context.Context {
	return s.internal.es.ctx
}

// Type describes custom type names and fields. Structs without it are traversed
// using their Go fields, including unexported fields.
//
// All these methods can be automatically generated by the go_stateify tool.
type Type interface {
	// StateTypeName returns the type's name.
	//
	// This is used for matching type information during encoding and
	// decoding, as well as dynamic interface dispatch. This should be
	// globally unique.
	StateTypeName() string

	// StateFields returns information about the type.
	//
	// Fields is the set of fields for the object. Calls to Sink.Save and
	// Source.Load must be made in-order with respect to these fields.
	//
	// This will be called at most once per serialization.
	StateFields() []string
}

// SaverLoader customizes saving and loading for structs implementing Type.
type SaverLoader interface {
	// StateSave saves the state of the object to the given Map.
	StateSave(Sink)

	// StateLoad loads the state of the object.
	StateLoad(context.Context, Source)
}

// Source is used for Type.StateLoad.
type Source struct {
	internal objectDecoder
}

// Load loads the given object passed as a pointer..
//
// See Sink.Save for an example.
func (s Source) Load(slot int, objPtr any) {
	s.internal.load(slot, reflect.ValueOf(objPtr), false, nil)
}

// LoadWait loads the given objects from the map, and marks it as requiring all
// AfterLoad executions to complete prior to running this object's AfterLoad.
//
// See Sink.Save for an example.
func (s Source) LoadWait(slot int, objPtr any) {
	s.internal.load(slot, reflect.ValueOf(objPtr), true, nil)
}

// LoadValue loads the given object value from the map.
//
// See Sink.SaveValue for an example.
func (s Source) LoadValue(slot int, objPtr any, fn func(any)) {
	o := reflect.ValueOf(objPtr)
	s.internal.load(slot, o, true, func() { fn(o.Elem().Interface()) })
}

// AfterLoad schedules a function execution when all objects have been
// allocated and their automated loading and customized load logic have been
// executed. fn will not be executed until all of current object's
// dependencies' AfterLoad() logic, if exist, have been executed.
func (s Source) AfterLoad(fn func()) {
	s.internal.afterLoad(fn)
}

// Context returns the context object provided at load time.
func (s Source) Context() context.Context {
	return s.internal.ds.ctx
}

// IsZeroValue checks if the given value is the zero value.
//
// This function is used by the stateify tool.
func IsZeroValue(val any) bool {
	return val == nil || reflect.ValueOf(val).Elem().IsZero()
}

// Failf is a wrapper around panic that should be used to generate errors that
// can be caught during saving and loading.
func Failf(fmtStr string, v ...any) {
	panic(fmt.Errorf(fmtStr, v...))
}

// safely executes the given function, catching a panic and unpacking as an
// error.
//
// The error flow through the state package uses panic and recover. There are
// two important reasons for this:
//
// 1) Many of the reflection methods will already panic with invalid data or
// violated assumptions. We would want to recover anyways here.
//
// 2) It allows us to eliminate boilerplate within Save() and Load() functions.
// In nearly all cases, when the low-level serialization functions fail, you
// will want the checkpoint to fail anyways. Plumbing errors through every
// method doesn't add a lot of value. If there are specific error conditions
// that you'd like to handle, you should add appropriate functionality to
// objects themselves prior to calling Save() and Load().
func safely(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if es, ok := r.(*ErrState); ok {
				err = es // Propagate.
				return
			}

			// Build a new state error.
			es := new(ErrState)
			if e, ok := r.(error); ok {
				es.err = e
			} else {
				es.err = fmt.Errorf("%v", r)
			}

			// Make a stack. We don't know how big it will be ahead
			// of time, but want to make sure we get the whole
			// thing. So we just do a stupid brute force approach.
			var stack []byte
			for sz := 1024; ; sz *= 2 {
				stack = make([]byte, sz)
				n := runtime.Stack(stack, false)
				if n < sz {
					es.trace = string(stack[:n])
					break
				}
			}

			// Set the error.
			err = es
		}
	}()

	// Execute the function.
	fn()
	return nil
}
