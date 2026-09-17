package state

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

type arrayByte uint8

func TestArrayCodecThreshold(t *testing.T) {
	for _, elem := range []reflect.Type{
		reflect.TypeFor[int](), reflect.TypeFor[int8](), reflect.TypeFor[int16](), reflect.TypeFor[int32](), reflect.TypeFor[int64](),
		reflect.TypeFor[uint](), reflect.TypeFor[uint8](), reflect.TypeFor[uint16](), reflect.TypeFor[uint32](), reflect.TypeFor[uint64](), reflect.TypeFor[uintptr](),
		reflect.TypeFor[float32](), reflect.TypeFor[float64](), reflect.TypeFor[complex64](), reflect.TypeFor[complex128](),
		reflect.TypeFor[arrayByte](),
	} {
		for _, length := range []int{63, 64, 65} {
			t.Run(fmt.Sprintf("%s/%d", elem, length), func(t *testing.T) {
				typ := reflect.ArrayOf(length, elem)
				src := reflect.New(typ).Elem()
				for i := range length {
					v := src.Index(i)
					switch elem.Kind() {
					case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
						v.SetInt(int64(i - 32))
					case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
						v.SetUint(uint64(i))
					case reflect.Float32, reflect.Float64:
						v.SetFloat(float64(i) - 32.5)
					case reflect.Complex64, reflect.Complex128:
						v.SetComplex(complex(float64(i), -0.5))
					}
				}
				mem := make([]byte, 2*int(typ.Size())+64)
				es := newEncodeState(context.Background(), mem)
				var value object
				es.encodeArray(src, &value)
				if _, bulk := value.(*rawArrayValue); bulk != (length >= 64) {
					t.Fatalf("length %d encoded as %T", length, value)
				}
				if err := es.w.put(value); err != nil {
					t.Fatal(err)
				}
				ds := newDecodeState(context.Background(), mem[:es.w.pos])
				encoded, err := ds.r.get()
				if err != nil {
					t.Fatal(err)
				}
				dst := reflect.New(typ).Elem()
				ds.decodeObject(&objectDecodeState{obj: dst}, dst, encoded)
				clear(mem)
				if !reflect.DeepEqual(src.Interface(), dst.Interface()) {
					t.Fatal("decoded array lost values or aliases the input buffer")
				}
			})
		}
	}
}

func TestRawArrayFloatBits(t *testing.T) {
	var f32 [64]float32
	var f64 [64]float64
	var c64 [64]complex64
	var c128 [64]complex128
	for i := range f32 {
		a := math.Float32frombits([]uint32{0x80000000, 0x7fc01234, 0x7f801234, 0xff800000}[i%4])
		b := math.Float64frombits([]uint64{0x8000000000000000, 0x7ff8000000001234, 0x7ff0000000001234, 0xfff0000000000000}[i%4])
		f32[i], f64[i] = a, b
		c64[i], c128[i] = complex(a, -a), complex(b, -b)
	}
	for _, src := range []any{&f32, &f64, &c64, &c128} {
		value := reflect.ValueOf(src).Elem()
		t.Run(value.Type().String(), func(t *testing.T) {
			dst := reflect.New(value.Type())
			roundtrip(t, src, dst.Interface())
			if !bytes.Equal(arrayBytes(value), arrayBytes(dst.Elem())) {
				t.Fatal("NaN payload or signed zero changed")
			}
		})
	}
}

func TestArrayCodecRecursiveElements(t *testing.T) {
	for _, elem := range []reflect.Type{
		reflect.TypeFor[bool](), reflect.TypeFor[string](), reflect.TypeFor[*int](),
		reflect.TypeFor[any](), reflect.TypeFor[struct{ N uint64 }](), reflect.TypeFor[atomic.Int64](),
	} {
		t.Run(elem.String(), func(t *testing.T) {
			src := reflect.New(reflect.ArrayOf(64, elem)).Elem()
			es := newEncodeState(context.Background(), nil)
			var value object
			es.encodeArray(src, &value)
			if _, ok := value.(*arrayValue); !ok {
				t.Fatalf("recursive elements encoded as %T", value)
			}
		})
	}
	values := make([]any, 64)
	for i := range values {
		values[i] = &i
	}
	var got []any
	roundtrip(t, &values, &got)
	for i := range values {
		if *got[i].(*int) != i || got[i] == values[i] {
			t.Fatal("pointer-containing elements lost relocation or values")
		}
	}
}

func TestRawArrayInvalidDestination(t *testing.T) {
	for _, target := range []any{[64]*int{}, [64]string{}, [64]any{}, [64]bool{}, [64]struct{ N uint64 }{}, uint64(42), [64]uint64{1}} {
		typ := reflect.TypeOf(target)
		for _, delta := range []int{-1, 0, 1} {
			if typ == reflect.TypeFor[[64]uint64]() && delta == 0 {
				continue
			}
			t.Run(fmt.Sprintf("%s/%d", typ, delta), func(t *testing.T) {
				dst := reflect.New(typ).Elem()
				dst.Set(reflect.ValueOf(target))
				data := bytes.Repeat([]byte{0xff}, int(typ.Size())+delta)
				ds := newDecodeState(context.Background(), nil)
				err := safely(func() {
					ds.decodeObject(&objectDecodeState{obj: dst}, dst, &rawArrayValue{Data: data})
				})
				if err == nil || !strings.Contains(err.Error(), "raw array") {
					t.Fatalf("invalid raw destination: %v", err)
				}
				if !reflect.DeepEqual(target, dst.Interface()) {
					t.Fatal("invalid raw data changed destination")
				}
			})
		}
	}
}

func TestBulkArrayMapValues(t *testing.T) {
	src := map[[64]byte][65]int{{1, 2, 3}: {4, 5, 6}}
	var got map[[64]byte][65]int
	roundtrip(t, &src, &got)
	if !reflect.DeepEqual(src, got) {
		t.Fatal("unaddressable array key or value changed")
	}
	private := struct{ data [64]byte }{data: [64]byte{1, 2, 3}}
	var restored struct{ data [64]byte }
	roundtrip(t, &private, &restored)
	if private != restored {
		t.Fatal("unexported array field changed")
	}
}

func TestBulkSliceAliasesAndWriteback(t *testing.T) {
	for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			backing := make([]uint64, 128)
			for i := range backing {
				backing[i] = uint64(i)
			}
			values := [3]any{backing[:4], backing[16:20:96], &backing[32]}
			host := [3]any{values[order[0]], values[order[1]], values[order[2]]}
			var guest [3]any
			var source, destination State
			mem := make([]byte, 1<<20)
			ctx := context.Background()
			n, _, err := source.Save(ctx, mem, &host)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := destination.Load(ctx, mem[:n], &guest); err != nil {
				t.Fatal(err)
			}
			clear(mem)
			var restored [3]any
			for i, index := range order {
				restored[index] = guest[i]
			}
			whole, sub, pointer := restored[0].([]uint64), restored[1].([]uint64), restored[2].(*uint64)
			if len(whole) != 4 || cap(whole) != 128 || len(sub) != 4 || cap(sub) != 80 {
				t.Fatal("slice length or capacity changed")
			}
			if &sub[0] != &whole[:cap(whole)][16] || pointer != &whole[:cap(whole)][32] || &whole[0] == &backing[0] {
				t.Fatal("backing array aliases, interior pointer or isolation lost")
			}
			if !reflect.DeepEqual(whole[:cap(whole)], backing) {
				t.Fatal("capacity tail was not copied")
			}
			*pointer, sub[0], whole[:cap(whole)][127] = 900, 901, 902
			if backing[32] != 32 || backing[16] != 16 || backing[127] != 127 {
				t.Fatal("guest changed host before writeback")
			}
			n, _, err = destination.Save(ctx, mem, &guest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := source.Load(ctx, mem[:n], &host); err != nil {
				t.Fatal(err)
			}
			clear(mem)
			if backing[32] != 900 || backing[16] != 901 || backing[127] != 902 {
				t.Fatal("guest changes were not written back")
			}
			for i, index := range order {
				switch value := host[i].(type) {
				case []uint64:
					if &value[0] != &values[index].([]uint64)[0] {
						t.Fatal("writeback replaced original backing array")
					}
				case *uint64:
					if value != &backing[32] {
						t.Fatal("writeback replaced original interior pointer")
					}
				}
			}
		})
	}
}
