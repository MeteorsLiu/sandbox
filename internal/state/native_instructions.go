package state

import (
	"debug/elf"

	"golang.org/x/arch/arm64/arm64asm"
	"golang.org/x/arch/x86/x86asm"
)

// closureAllocations recognizes newobject(type) immediately followed by a
// store of a function entry to offset zero of the returned object. Only this
// validated sequence associates a closure PC with an environment descriptor.
func closureAllocations(machine elf.Machine, code []byte, start, newobject uint64) map[uintptr]uintptr {
	found := make(map[uintptr]uintptr)
	switch machine {
	case elf.EM_AARCH64:
		var ins []arm64asm.Inst
		var pcs []uint64
		for off := 0; off+4 <= len(code); off += 4 {
			i, err := arm64asm.Decode(code[off:])
			if err != nil {
				// Alignment padding at the end of Go functions is not code.
				break
			}
			if i.Op != arm64asm.NOP {
				ins = append(ins, i)
				pcs = append(pcs, start+uint64(off))
			}
		}
		for i := 2; i+3 < len(ins); i++ {
			call := ins[i]
			rel, ok := call.Args[0].(arm64asm.PCRel)
			if call.Op != arm64asm.BL || !ok || uint64(int64(pcs[i])+int64(rel)) != newobject {
				continue
			}
			addr, argReg, ok := armAddress(ins[i-2], ins[i-1], pcs[i-2])
			if !ok || argReg != arm64asm.X0 {
				continue
			}
			pc, pcReg, ok := armAddress(ins[i+1], ins[i+2], pcs[i+1])
			if !ok || pcReg == arm64asm.X0 {
				continue
			}
			j := i + 3
			// A scalar capture can be reloaded before STP writes both PC and
			// capture, e.g. LDR X2,[SP,#56]; STP X1,X2,[X0].
			load := ins[j]
			reg, _ := armReg(load.Args[0])
			mem, stackLoad := load.Args[1].(arm64asm.MemImmediate)
			if load.Op == arm64asm.LDR && stackLoad && mem.Base == arm64asm.RegSP(arm64asm.SP) && mem.Mode == arm64asm.AddrOffset && reg != arm64asm.X0 && reg != pcReg {
				j++
			}
			if j >= len(ins) {
				continue
			}
			store := ins[j]
			storedReg, _ := armReg(store.Args[0])
			var offset uint32
			switch store.Op {
			case arm64asm.STR:
				mem, ok = store.Args[1].(arm64asm.MemImmediate)
				offset = (store.Enc >> 10) & 0xfff
			case arm64asm.STP:
				mem, ok = store.Args[2].(arm64asm.MemImmediate)
				offset = (store.Enc >> 15) & 0x7f
			default:
				continue
			}
			if storedReg != pcReg || !ok || mem.Base != arm64asm.RegSP(arm64asm.X0) || mem.Mode != arm64asm.AddrOffset || offset != 0 {
				continue
			}
			found[uintptr(pc)] = uintptr(addr)
		}
	case elf.EM_X86_64:
		var ins []x86asm.Inst
		var pcs []uint64
		for off := 0; off < len(code); {
			i, err := x86asm.Decode(code[off:], 64)
			if err != nil {
				break
			}
			if i.Op != x86asm.NOP {
				ins = append(ins, i)
				pcs = append(pcs, start+uint64(off))
			}
			off += i.Len
		}
		for i := 1; i+2 < len(ins); i++ {
			call := ins[i]
			rel, ok := call.Args[0].(x86asm.Rel)
			if call.Op != x86asm.CALL || !ok || uint64(int64(pcs[i])+int64(call.Len)+int64(rel)) != newobject {
				continue
			}
			a, c, store := ins[i-1], ins[i+1], ins[i+2]
			am, ok := a.Args[1].(x86asm.Mem)
			if a.Op != x86asm.LEA || a.Args[0] != x86asm.RAX || !ok || am.Base != x86asm.RIP || am.Index != 0 {
				continue
			}
			cm, ok := c.Args[1].(x86asm.Mem)
			if c.Op != x86asm.LEA || !ok || cm.Base != x86asm.RIP || cm.Index != 0 || c.Args[0] == x86asm.RAX {
				continue
			}
			sm, ok := store.Args[0].(x86asm.Mem)
			if store.Op != x86asm.MOV || store.MemBytes != 8 || !ok || sm.Base != x86asm.RAX || sm.Index != 0 || sm.Disp != 0 || store.Args[1] != c.Args[0] {
				continue
			}
			addr := uint64(int64(pcs[i-1]) + int64(a.Len) + am.Disp)
			pc := uint64(int64(pcs[i+1]) + int64(c.Len) + cm.Disp)
			found[uintptr(pc)] = uintptr(addr)
		}
	}
	return found
}

func armReg(arg arm64asm.Arg) (arm64asm.Reg, bool) {
	switch r := arg.(type) {
	case arm64asm.Reg:
		return r, true
	case arm64asm.RegSP:
		return arm64asm.Reg(r), true
	}
	return 0, false
}

func armAddress(a, b arm64asm.Inst, pc uint64) (uint64, arm64asm.Reg, bool) {
	reg, ok := armReg(a.Args[0])
	if !ok || a.Op != arm64asm.ADRP || b.Op != arm64asm.ADD {
		return 0, 0, false
	}
	rel, ok := a.Args[1].(arm64asm.PCRel)
	if !ok {
		return 0, 0, false
	}
	dst, _ := armReg(b.Args[0])
	src, _ := armReg(b.Args[1])
	if dst != reg || src != reg {
		return 0, 0, false
	}
	if _, ok := b.Args[2].(arm64asm.ImmShift); !ok {
		return 0, 0, false
	}
	// ImmShift's immediate is private in arm64asm; ADD encodes imm12 in 10..21.
	imm := uint64((b.Enc>>10)&0xfff) << (((b.Enc >> 22) & 1) * 12)
	return uint64(int64(pc&^4095)+int64(rel)) + imm, reg, true
}
