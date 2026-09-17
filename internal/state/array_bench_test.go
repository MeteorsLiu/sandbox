package state

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func BenchmarkArrayCodec(b *testing.B) {
	for _, elem := range []reflect.Type{reflect.TypeFor[byte](), reflect.TypeFor[uint64]()} {
		for _, length := range []int{16, 64, 1024, 65536} {
			typ := reflect.ArrayOf(length, elem)
			src := reflect.New(typ).Elem()
			for i := range length {
				src.Index(i).SetUint(uint64(i & 255))
			}
			b.Run(fmt.Sprintf("%s/%d", elem, length), func(b *testing.B) {
				mem := make([]byte, 2*int(typ.Size())+64)
				es := newEncodeState(context.Background(), mem)
				var encoded object
				es.encodeArray(src, &encoded)
				if err := es.w.put(encoded); err != nil {
					b.Fatal(err)
				}
				image := mem[:es.w.pos]
				b.Run("encode", func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(typ.Size()))
					for b.Loop() {
						es.w.pos = 0
						es.encodeArray(src, &encoded)
						if err := es.w.put(encoded); err != nil {
							b.Fatal(err)
						}
					}
				})
				b.Run("decode", func(b *testing.B) {
					ds := newDecodeState(context.Background(), image)
					dst := reflect.New(typ).Elem()
					ods := &objectDecodeState{obj: dst}
					b.ReportAllocs()
					b.SetBytes(int64(typ.Size()))
					for b.Loop() {
						ds.r.pos = 0
						value, err := ds.r.get()
						if err != nil {
							b.Fatal(err)
						}
						ds.decodeObject(ods, dst, value)
					}
				})
			})
		}
	}
}
