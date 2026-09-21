package state

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"

	"golang.org/x/arch/arm64/arm64asm"
	"golang.org/x/arch/x86/x86asm"
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

func TestClosureAllocationCandidates(t *testing.T) {
	for _, test := range []struct {
		name    string
		machine elf.Machine
		code    string
		start   uint64
		want    bool
	}{
		// CASALW in ixgo.newInterp is not recognized by arm64asm v0.28.0.
		// It precedes an otherwise supported allocation of PC 0x4000, type 0x3000.
		{"arm64-unknown-before", elf.EM_AARCH64, "c5fcfb88000000d000000091fe030094010000f021000091010000f9", 0xffc, true},
		{"arm64-unknown-after", elf.EM_AARCH64, "000000d000000091fe030094010000f021000091010000f9c5fcfb88", 0x1000, true},
		{"arm64-unknown-inside", elf.EM_AARCH64, "000000d000000091fe030094010000f021000091c5fcfb88010000f9", 0x1000, false},
		{"arm64-nop", elf.EM_AARCH64, "000000d01f2003d500000091fd030094010000f021000091010000f9", 0x1000, true},
		{"amd64-nop", elf.EM_X86_64, "488d05f91f000090e8f30f0000488d0dec2f0000488908", 0x1000, true},
		// MOVABS RAX, imm64 contains both the apparent LEA and the E8 byte.
		// Even a correct relative target and matching suffix cannot make it a CALL.
		{"amd64-call-in-immediate", elf.EM_X86_64, "48b8488d05f91f0000e8f40f0000488d0ded2f0000488908", 0xffe, false},
		{"amd64-result-overwritten", elf.EM_X86_64, "488d05f91f0000e8f40f00004889d8488d0dea2f0000488908", 0x1000, false},
		{"amd64-truncated-call", elf.EM_X86_64, "488d05f91f0000e8f40f00", 0x1000, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, err := hex.DecodeString(test.code)
			if err != nil {
				t.Fatal(err)
			}
			got := closureAllocations(test.machine, code, test.start, 0x2000)
			if test.want {
				if len(got) != 1 || got[0x4000] != 0x3000 {
					t.Fatalf("allocation: %#v", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("accepted invalid allocation: %#v", got)
			}
		})
	}
}

func TestClosureAllocationRegisterLiveness(t *testing.T) {
	for _, test := range []struct {
		name          string
		machine       elf.Machine
		before, after string
		want          bool
	}{
		// LDR W3,[SP,#228]; AND X3,X3,#0xffffff, as in ixgo.makeInstr.
		{"arm64-capture-before-pc", elf.EM_AARCH64, "e3e740b9635c4092", "", true},
		{"arm64-capture-after-pc", elf.EM_AARCH64, "", "e3e740b9635c4092", true},
		// TestImplicitMapConversion initializes reflect.Value.flag before F:
		// LDR; CMP; MOV; MOV; CSEL; STR to stack; STR to the capture at +24.
		{"arm64-reflect-capture-before-pc", elf.EM_AARCH64, "e14740b93f000071a10280d2a21280d22110829ae14300f9010c00f9", "", true},
		{"arm64-select-after-pc", elf.EM_AARCH64, "", "8310859a", true},
		{"arm64-compare", elf.EM_AARCH64, "", "1f0001eb", true},
		{"arm64-pair-load", elf.EM_AARCH64, "", "e20f40a9", true},
		{"arm64-overwrite-object-before-pc", elf.EM_AARCH64, "e003032a", "", false},
		{"arm64-overwrite-object-after-pc", elf.EM_AARCH64, "", "e003032a", false},
		{"arm64-overwrite-pc", elf.EM_AARCH64, "", "e103032a", false},
		// LDP's second destination and post-index LDR's base also get written.
		{"arm64-pair-overwrite-object", elf.EM_AARCH64, "", "e20340a9", false},
		{"arm64-pair-overwrite-pc", elf.EM_AARCH64, "", "e20740a9", false},
		{"arm64-post-index-object", elf.EM_AARCH64, "", "038440f8", false},
		{"arm64-post-index-pc", elf.EM_AARCH64, "", "238440f8", false},
		{"arm64-branch", elf.EM_AARCH64, "", "02000014", false},
		{"arm64-conditional-branch", elf.EM_AARCH64, "", "430000b4", false},
		{"arm64-call", elf.EM_AARCH64, "", "02000094", false},
		{"arm64-unknown", elf.EM_AARCH64, "", "c5fcfb88", false},
		{"arm64-spill", elf.EM_AARCH64, "", "e10300f9", true},
		{"arm64-spill-reload-object", elf.EM_AARCH64, "", "e10300f9e00340f9", false},
		{"arm64-capture-store", elf.EM_AARCH64, "", "030c00f9", true},
		{"arm64-capture-pair-store", elf.EM_AARCH64, "", "020c01a9", true},
		{"arm64-store-post-index-other", elf.EM_AARCH64, "", "438400f8", true},
		{"arm64-store-post-index-object", elf.EM_AARCH64, "", "038400f8", false},
		{"arm64-store-pre-index-object", elf.EM_AARCH64, "", "038c00f8", false},
		{"arm64-store-post-index-pc", elf.EM_AARCH64, "", "238400f8", false},
		{"arm64-pair-store-post-index-object", elf.EM_AARCH64, "", "020c81a8", false},
		{"arm64-pair-store-pre-index-object", elf.EM_AARCH64, "", "020c81a9", false},
		{"arm64-pair-store-pre-index-pc", elf.EM_AARCH64, "", "220c81a9", false},
		// MOV EDX,[RSP+8]; AND EDX,0xffffff only affect the scalar capture.
		{"amd64-capture-before-pc", elf.EM_X86_64, "8b54240881e2ffffff00", "", true},
		{"amd64-capture-after-pc", elf.EM_X86_64, "", "8b54240881e2ffffff00", true},
		{"amd64-select-before-pc", elf.EM_X86_64, "480f45d3", "", true},
		{"amd64-select-after-pc", elf.EM_X86_64, "", "480f45d3", true},
		{"amd64-compare", elf.EM_X86_64, "", "4839c8", true},
		{"amd64-explicit-multiply", elf.EM_X86_64, "", "0fafd2", true},
		{"amd64-overwrite-object-before-pc", elf.EM_X86_64, "31c0", "", false},
		{"amd64-overwrite-eax", elf.EM_X86_64, "", "31c0", false},
		{"amd64-overwrite-ax", elf.EM_X86_64, "", "6631c0", false},
		{"amd64-overwrite-al", elf.EM_X86_64, "", "b001", false},
		{"amd64-overwrite-ah", elf.EM_X86_64, "", "b401", false},
		{"amd64-overwrite-ecx", elf.EM_X86_64, "", "31c9", false},
		{"amd64-overwrite-ch", elf.EM_X86_64, "", "b501", false},
		{"amd64-implicit-multiply", elf.EM_X86_64, "", "f7e2", false},
		{"amd64-implicit-signed-multiply", elf.EM_X86_64, "", "f7ea", false},
		{"amd64-exchange", elf.EM_X86_64, "", "4887d0", false},
		{"amd64-branch", elf.EM_X86_64, "", "eb03", false},
		{"amd64-conditional-branch", elf.EM_X86_64, "", "7403", false},
		{"amd64-call", elf.EM_X86_64, "", "e800000000", false},
		{"amd64-unknown", elf.EM_X86_64, "", "06", false},
		{"amd64-spill", elf.EM_X86_64, "", "48890c24", true},
		{"amd64-spill-reload-object", elf.EM_X86_64, "", "48890c24488b0424", false},
		{"amd64-capture-store", elf.EM_X86_64, "", "48895018", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			head, address, store := "000000d000000091fe030094", "010000f021000091", "010000f9"
			if test.machine == elf.EM_X86_64 {
				head, address, store = "488d05f91f0000e8f40f0000", "488d0d00000000", "488908"
			}
			code, err := hex.DecodeString(head + test.before + address + test.after + store)
			if err != nil {
				t.Fatal(err)
			}
			if test.machine == elf.EM_X86_64 {
				off := (len(head) + len(test.before)) / 2
				binary.LittleEndian.PutUint32(code[off+3:], uint32(0x4000-(0x1000+off+7)))
			}
			got := closureAllocations(test.machine, code, 0x1000, 0x2000)
			if test.want {
				if len(got) != 1 || got[0x4000] != 0x3000 {
					t.Fatalf("allocation: %#v", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("accepted overwritten or unproven register: %#v", got)
			}
		})
	}
}

func TestClosureConditionalRegisters(t *testing.T) {
	for _, test := range []struct {
		op   arm64asm.Op
		word uint32
	}{
		{arm64asm.CSEL, 0x9a851083},
		{arm64asm.CSINC, 0x9a851483},
		{arm64asm.CSINV, 0xda851083},
		{arm64asm.CSNEG, 0xda851483},
		{arm64asm.CINC, 0x9a840483},
		{arm64asm.CINV, 0xda840083},
		{arm64asm.CNEG, 0xda840483},
		{arm64asm.CSET, 0x9a9f07e3},
		{arm64asm.CSETM, 0xda9f03e3},
	} {
		for _, width := range []int{32, 64} {
			for _, dst := range []uint32{0, 1, 3} {
				t.Run(fmt.Sprintf("arm64/%s/%d/R%d", test.op, width, dst), func(t *testing.T) {
					word := test.word&^31 | dst
					if width == 32 {
						word &^= 1 << 31
					}
					var code [4]byte
					binary.LittleEndian.PutUint32(code[:], word)
					inst, err := arm64asm.Decode(code[:])
					if err != nil || inst.Op != test.op {
						t.Fatalf("decode: %v, %v", inst, err)
					}
					if got := armPreservesRegisters(inst, arm64asm.X1); got != (dst == 3) {
						t.Fatalf("%s preserves object X0 and PC X1: %v", inst, got)
					}
				})
			}
		}
	}
	// CMOVcc covers all 16 conditions; 16/32-bit destinations also invalidate
	// their tracked 64-bit register. A memory source does not change its base.
	for condition := byte(0); condition < 16; condition++ {
		for _, width := range []int{16, 32, 64} {
			for _, dst := range []byte{0, 1, 3} {
				for _, memory := range []bool{false, true} {
					t.Run(fmt.Sprintf("amd64/condition%d/%d/R%d/memory%v", condition, width, dst, memory), func(t *testing.T) {
						var code []byte
						if width == 16 {
							code = append(code, 0x66)
						} else if width == 64 {
							code = append(code, 0x48)
						}
						modrm := byte(0xc2) | dst<<3 // Source RDX/EDX/DX.
						if memory {
							modrm = dst << 3 // Source [RAX].
						}
						code = append(code, 0x0f, 0x40+condition, modrm)
						inst, err := x86asm.Decode(code, 64)
						if err != nil {
							t.Fatal(err)
						}
						if got := x86PreservesRegisters(inst, x86asm.RCX); got != (dst == 3) {
							t.Fatalf("%s preserves object RAX and PC RCX: %v", inst, got)
						}
					})
				}
			}
		}
	}
}

func BenchmarkClosureAllocations(b *testing.B) {
	for _, test := range []struct {
		name    string
		machine elf.Machine
		code    string
		nop     []byte
	}{
		{"amd64", elf.EM_X86_64, "488d05f91f0000e8f40f0000488d0ded2f0000488908", []byte{0x90}},
		{"arm64", elf.EM_AARCH64, "000000d000000091fe030094010000f021000091010000f9", []byte{0x1f, 0x20, 0x03, 0xd5}},
	} {
		allocation, err := hex.DecodeString(test.code)
		if err != nil {
			b.Fatal(err)
		}
		padding := bytes.Repeat(test.nop, 4096/len(test.nop))
		for _, position := range []string{"before", "after"} {
			b.Run(test.name+"/"+position, func(b *testing.B) {
				var code []byte
				var start uint64
				if position == "before" {
					code = append(append([]byte(nil), padding...), allocation...)
				} else {
					start = 0x1000
					code = append(append([]byte(nil), allocation...), padding...)
				}
				b.ReportAllocs()
				for b.Loop() {
					got := closureAllocations(test.machine, code, start, 0x2000)
					if len(got) != 1 || got[0x4000] != 0x3000 {
						b.Fatalf("allocation: %#v", got)
					}
				}
			})
		}
	}
}
