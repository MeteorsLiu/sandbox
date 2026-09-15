package main

import (
	"bytes"
	"testing"
)

func TestEntryJump(t *testing.T) {
	for _, test := range []struct {
		name     string
		from, to uintptr
		want     []byte
	}{
		{"forward", 0x1000, 0x2000, []byte{0xe9, 0xfb, 0x0f, 0, 0}},
		{"backward", 0x2000, 0x1000, []byte{0xe9, 0xfb, 0xef, 0xff, 0xff}},
		{"minimum", 0x80001000, 0x1005, []byte{0xe9, 0, 0, 0, 0x80}},
		{"maximum", 0x1000, 0x80001004, []byte{0xe9, 0xff, 0xff, 0xff, 0x7f}},
		{"below-minimum", 0x80001000, 0x1004, nil},
		{"above-maximum", 0x1000, 0x80001005, nil},
		{"missing-main", 0, 0x1000, nil},
		{"missing-entry", 0x1000, 0, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := entryJump(test.from, test.to)
			if test.want == nil {
				if err == nil {
					t.Fatal("invalid jump accepted")
				}
				return
			}
			if err != nil || !bytes.Equal(got, test.want) {
				t.Fatalf("entryJump = %x, %v; want %x", got, err, test.want)
			}
		})
	}
}
