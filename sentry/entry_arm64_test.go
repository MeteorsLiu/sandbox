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
		{"forward", 0x1000, 0x2000, []byte{0, 4, 0, 0x14}},
		{"backward", 0x2000, 0x1000, []byte{0, 0xfc, 0xff, 0x17}},
		{"minimum", 0x08001000, 0x1000, []byte{0, 0, 0, 0x16}},
		{"maximum", 0x1000, 0x08000ffc, []byte{0xff, 0xff, 0xff, 0x15}},
		{"below-minimum", 0x08001004, 0x1000, nil},
		{"above-maximum", 0x1000, 0x08001000, nil},
		{"unaligned-main", 0x1001, 0x2000, nil},
		{"unaligned-entry", 0x1000, 0x2001, nil},
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
