package state

import (
	"debug/elf"
	"encoding/hex"
	"testing"
)

func TestClosureAllocationInstructions(t *testing.T) {
	for _, test := range []struct {
		name    string
		machine elf.Machine
		code    string
		store   int
	}{
		{"amd64", elf.EM_X86_64, "488d05f91f0000e8f40f0000488d0ded2f0000488908", 19},
		{"arm64", elf.EM_AARCH64, "000000d000000091fe030094010000f021000091010000f9", 20},
		{"arm64-paired", elf.EM_AARCH64, "000000d000000091fe030094010000f021000091e21f40f9010800a9", 24},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, err := hex.DecodeString(test.code)
			if err != nil {
				t.Fatal(err)
			}
			// newobject(0x3000) returns the object; 0x4000 is stored at +0.
			got := closureAllocations(test.machine, code, 0x1000, 0x2000)
			if len(got) != 1 || got[0x4000] != 0x3000 {
				t.Fatalf("allocation: %#v", got)
			}
			if got := closureAllocations(test.machine, code, 0x1000, 0x2004); len(got) != 0 {
				t.Fatalf("accepted call to another function: %#v", got)
			}
			// Loading a PC alone is insufficient; it must initialize the object.
			if got := closureAllocations(test.machine, code[:test.store], 0x1000, 0x2000); len(got) != 0 {
				t.Fatalf("accepted allocation without PC store: %#v", got)
			}
			if test.machine == elf.EM_X86_64 {
				code[len(code)-1] = 0x0b // MOV [RBX], RCX instead of [RAX].
			} else {
				code[test.store] = 0x41 // STR X1, [X2] instead of [X0].
			}
			if got := closureAllocations(test.machine, code, 0x1000, 0x2000); len(got) != 0 {
				t.Fatalf("accepted store to a different object: %#v", got)
			}
		})
	}
}
