//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func TestRejectedOutputDoesNotCommit(t *testing.T) {
	m, err := loadMetadata()
	if err != nil {
		t.Fatal(err)
	}
	for _, corruption := range []string{"length", "code", "retained-type", "retained-count", "alias-merge"} {
		t.Run(corruption, func(t *testing.T) {
			a, b := 1, 2
			fn := func() { a = 10; b = 20 }
			functions := make(map[uintptr]nativeLayout)
			in := newImage(make([]byte, 1<<20), m, functions)
			w, err := in.encode(reflect.ValueOf(fn), nil)
			if err != nil {
				t.Fatal(err)
			}
			guest, retained, err := in.decode(nil)
			if err != nil {
				t.Fatal(err)
			}
			guest.Interface().(func())()
			out := newImage(make([]byte, 1<<20), m, functions)
			if _, err := out.encode(guest, retained); err != nil {
				t.Fatal(err)
			}
			switch corruption {
			case "length":
				binary.LittleEndian.PutUint64(out.mem[8:], uint64(len(out.mem)+1))
			case "code":
				header, _ := out.word(headerWord(out.mem, 0))
				binary.LittleEndian.PutUint64(out.mem[header+8:], 1)
			case "retained-type":
				table := headerWord(out.mem, 16)
				binary.LittleEndian.PutUint64(out.mem[table:], 1)
			case "retained-count":
				binary.LittleEndian.PutUint64(out.mem[24:], 0)
			case "alias-merge":
				table := headerWord(out.mem, 16)
				var first uintptr
				for i, v := range retained {
					if v.Type() != reflect.TypeFor[*int]() {
						continue
					}
					slot, _ := out.word(table + uintptr(i)*16 + 8)
					if first == 0 {
						first = slot
						continue
					}
					addr, _ := out.word(first)
					binary.LittleEndian.PutUint64(out.mem[slot:], uint64(addr))
				}
			}
			if err := out.commit(w.anchors); err == nil {
				t.Fatal("corrupted output accepted")
			}
			if a != 1 || b != 2 {
				t.Fatalf("partial host mutation: a=%d b=%d", a, b)
			}
		})
	}
}

func TestOverlappingSlicesRejected(t *testing.T) {
	m, err := loadMetadata()
	if err != nil {
		t.Fatal(err)
	}
	backing := []int{1, 2, 3, 4}
	a, b := backing[:3], backing[1:]
	fn := func() { a[1]++; b[0]++ }
	in := newImage(make([]byte, 1<<20), m, make(map[uintptr]nativeLayout))
	if _, err := in.encode(reflect.ValueOf(fn), nil); err == nil {
		t.Fatal("overlapping slice backing was copied independently")
	}
}
