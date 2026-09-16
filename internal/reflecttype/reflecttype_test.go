// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

package reflecttype_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"testing"

	"github.com/xgo-dev/sandbox/internal/reflecttype"
)

type Embedded struct {
	Value int
	Next  *Embedded
}

func (e Embedded) Number() int { return e.Value }

func sampleTypes() []reflect.Type {
	fields := []reflect.StructField{
		{Name: "Embedded", Type: reflect.TypeFor[Embedded](), Anonymous: true},
		{Name: "Count", Type: reflect.TypeFor[int](), Tag: `json:"count"`},
		{Name: "label", PkgPath: "reflecttype_test", Type: reflect.TypeFor[string]()},
	}
	for _, value := range []any{
		false, int8(0), int16(0), int32(0), int64(0), uint(0), uint8(0),
		uint16(0), uint32(0), uint64(0), uintptr(0), float32(0), float64(0),
		complex64(0), complex128(0),
	} {
		fields = append(fields, reflect.StructField{Name: fmt.Sprintf("Field%d", len(fields)), Type: reflect.TypeOf(value)})
	}
	item := reflect.StructOf(fields)
	return []reflect.Type{
		item, reflect.PointerTo(item), reflect.SliceOf(item), reflect.ArrayOf(3, item),
		reflect.MapOf(reflect.TypeFor[string](), item),
		reflect.ChanOf(reflect.BothDir, item), reflect.ChanOf(reflect.SendDir, item), reflect.ChanOf(reflect.RecvDir, item),
		reflect.FuncOf([]reflect.Type{item, reflect.SliceOf(reflect.TypeFor[int]())}, []reflect.Type{reflect.TypeFor[error]()}, true),
		reflect.FuncOf(nil, []reflect.Type{item}, false),
		reflect.TypeFor[Embedded](), reflect.TypeFor[error](),
	}
}

func TestExportOpenResolve(t *testing.T) {
	want := sampleTypes()
	snapshot, err := reflecttype.Export()
	if err != nil {
		t.Fatal(err)
	}
	types, err := reflecttype.Open(snapshot.Data)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[uint32]bool)
	for typ, id := range snapshot.IDs {
		if id == 0 || seen[id] {
			t.Fatalf("invalid or repeated ID %d for %v", id, typ)
		}
		seen[id] = true
		got, err := types.Resolve(id)
		if err != nil || got != typ {
			t.Fatalf("Resolve(%d) = %v, %v; want %v", id, got, err, typ)
		}
	}
	for _, typ := range want {
		if snapshot.IDs[typ] == 0 {
			t.Errorf("missing discovered type or dependency %v", typ)
		}
	}
	for _, id := range []uint32{0, uint32(len(snapshot.IDs)) + 1, math.MaxUint32} {
		if _, err := types.Resolve(id); err == nil {
			t.Errorf("Resolve(%d) accepted an invalid ID", id)
		}
	}
	// Open must own its decoded types even after the transport buffer is reused.
	clear(snapshot.Data)
	id := snapshot.IDs[want[0]]
	got, err := types.Resolve(id)
	if err != nil || got != want[0] {
		t.Fatalf("Resolve after buffer reuse: %v, %v", got, err)
	}
}

func TestExportStaticDependencies(t *testing.T) {
	typ := reflect.TypeFor[reflect.Type]()
	reflect.SliceOf(typ)
	snapshot, err := reflecttype.Export()
	if err != nil {
		t.Fatal(err)
	}
	id := snapshot.IDs[typ]
	if id == 0 {
		t.Fatal("static cached slice omitted its element type ID")
	}
	types, err := reflecttype.Open(snapshot.Data)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := types.Resolve(id); err != nil || got != typ {
		t.Fatalf("Resolve = %v, %v; want %v", got, err, typ)
	}
}

func TestOpenInFreshProcess(t *testing.T) {
	if os.Getenv("SANDBOX_REFLECTTYPE_CHILD") == "1" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		types, err := reflecttype.Open(data[4:])
		if err != nil {
			t.Fatal(err)
		}
		got, err := types.Resolve(binary.LittleEndian.Uint32(data[:4]))
		if err != nil {
			t.Fatal(err)
		}
		// Construct the expectation only after Open, so the receiving process
		// cannot satisfy reconstruction from types created by the fixture.
		if want := sampleTypes()[0]; got != want {
			t.Fatalf("got %v, want %v", got, want)
		}
		value := reflect.New(got).Elem()
		value.FieldByName("Count").SetInt(42)
		value.FieldByName("Embedded").Set(reflect.ValueOf(Embedded{Value: 7}))
		if value.FieldByName("Count").Int() != 42 || value.MethodByName("Number").Call(nil)[0].Int() != 7 {
			t.Fatal("restored fields or method set are incorrect")
		}
		return
	}
	want := sampleTypes()[0]
	snapshot, err := reflecttype.Export()
	if err != nil {
		t.Fatal(err)
	}
	data := binary.LittleEndian.AppendUint32(nil, snapshot.IDs[want])
	data = append(data, snapshot.Data...)
	cmd := exec.Command(os.Args[0], "-test.run=^TestOpenInFreshProcess$")
	cmd.Env = append(os.Environ(), "SANDBOX_REFLECTTYPE_CHILD=1")
	cmd.Stdin = bytes.NewReader(data)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guest: %v\n%s", err, output)
	}
}

func TestConcurrentTransfers(t *testing.T) {
	want := sampleTypes()[0]
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Go(func() {
			// Exercise cache Range while normal reflect constructors write caches.
			for n := 5; n < 10; n++ {
				reflect.ArrayOf(n, want)
			}
			snapshot, err := reflecttype.Export()
			if err != nil {
				t.Error(err)
				return
			}
			types, err := reflecttype.Open(snapshot.Data)
			if err != nil {
				t.Error(err)
				return
			}
			var readers sync.WaitGroup
			for i := 0; i < 4; i++ {
				readers.Go(func() {
					got, err := types.Resolve(snapshot.IDs[want])
					if err != nil || got != want {
						t.Errorf("Resolve: %v, %v", got, err)
					}
				})
			}
			readers.Wait()
		})
	}
	group.Wait()
}

// encodedTable builds independent malformed inputs without using the exporter.
func encodedTable(entries ...[]uint64) []byte {
	data := binary.AppendUvarint(nil, uint64(len(entries)))
	for _, values := range entries {
		var entry []byte
		for _, value := range values {
			entry = binary.AppendUvarint(entry, value)
		}
		data = binary.AppendUvarint(data, uint64(len(entry)))
		data = append(data, entry...)
	}
	return data
}

func TestOpenRejectsInvalidData(t *testing.T) {
	cases := map[string][]byte{
		"empty":                  nil,
		"truncated table":        {1},
		"truncated entry":        {1, 2, byte(reflect.Int)},
		"empty entry":            {1, 0},
		"trailing table":         {0, 1},
		"overflow integer":       bytes.Repeat([]byte{255}, 11),
		"unknown kind":           encodedTable([]uint64{255}),
		"trailing entry":         encodedTable([]uint64{uint64(reflect.Int), 0}),
		"invalid dependency":     encodedTable([]uint64{uint64(reflect.Slice), 2}),
		"zero dependency":        encodedTable([]uint64{uint64(reflect.Slice), 0}),
		"cycle":                  encodedTable([]uint64{uint64(reflect.Slice), 1}),
		"unknown static type":    encodedTable([]uint64{0, 0, math.MaxUint64}),
		"bad variadic flag":      encodedTable([]uint64{uint64(reflect.Func), 2, 0, 0}),
		"missing variadic input": encodedTable([]uint64{uint64(reflect.Func), 1, 0, 0}),
		"bad channel direction":  encodedTable([]uint64{uint64(reflect.Chan), 2, 4}, []uint64{uint64(reflect.Int)}),
		"array overflow":         encodedTable([]uint64{uint64(reflect.Array), 2, math.MaxUint64}, []uint64{uint64(reflect.Int)}),
		"noncomparable map key":  encodedTable([]uint64{uint64(reflect.Map), 2, 3}, []uint64{uint64(reflect.Slice), 3}, []uint64{uint64(reflect.Int)}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if result, err := reflecttype.Open(data); err == nil || result != nil {
				t.Fatalf("Open = %v, %v; want nil and an error", result, err)
			}
		})
	}
}
