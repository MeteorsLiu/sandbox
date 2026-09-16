package state

import (
	"bytes"
	"debug/elf"
	"encoding/binary"

	"golang.org/x/arch/arm64/arm64asm"
	"golang.org/x/arch/x86/x86asm"
)

// closureAllocations associates a type with a PC stored at offset zero of a
// newobject result. Instructions between allocation and store may compute
// captures, but must preserve the object and PC registers.
func closureAllocations(machine elf.Machine, code []byte, start, newobject uint64) map[uintptr]uintptr {
	found := make(map[uintptr]uintptr)
	switch machine {
	case elf.EM_AARCH64:
		for off := 0; off+4 <= len(code); off += 4 {
			// BL has a signed imm26 in units of four bytes. Unrelated
			// instructions need no decoding, including newer atomic opcodes.
			word := binary.LittleEndian.Uint32(code[off:])
			if word&0xfc000000 != 0x94000000 {
				continue
			}
			rel := int64(int32(word<<6)>>6) * 4
			if uint64(int64(start+uint64(off))+rel) != newobject {
				continue
			}
			b, bOff, ok := armInstruction(code, off-4, -4)
			if !ok {
				continue
			}
			a, aOff, ok := armInstruction(code, bOff-4, -4)
			if !ok {
				continue
			}
			addr, argReg, ok := armAddress(a, b, start+uint64(aOff))
			if !ok || argReg != arm64asm.X0 {
				continue
			}
			if pc, ok := armStoredClosurePC(code, off+4, start); ok {
				found[uintptr(pc)] = uintptr(addr)
			}
		}
	case elf.EM_X86_64:
		boundary := 0
		var previous x86asm.Inst
		var previousPC uint64
		for search := 0; search+5 <= len(code); {
			at := bytes.IndexByte(code[search:len(code)-4], 0xe8)
			if at < 0 {
				break
			}
			off := search + at
			search = off + 1
			rel := int64(int32(binary.LittleEndian.Uint32(code[off+1:])))
			if off < boundary || uint64(int64(start+uint64(off)+5)+rel) != newobject {
				continue
			}
			pc, ok := x86StoredClosurePC(code, off+5, start)
			if !ok {
				continue
			}
			// E8 can occur inside an immediate. Confirm instruction boundaries
			// from the function entry, advancing only for matching candidates.
			// A decode failure cannot be skipped on a variable-length ISA.
			for boundary < off {
				inst, err := x86asm.Decode(code[boundary:], 64)
				if err != nil || inst.Op == 0 || inst.Len == 0 {
					return found
				}
				if inst.Op != x86asm.NOP {
					previous, previousPC = inst, start+uint64(boundary)
				}
				boundary += inst.Len
			}
			if boundary != off {
				continue
			}
			am, ok := previous.Args[1].(x86asm.Mem)
			if previous.Op != x86asm.LEA || previous.AddrSize != 64 || previous.Args[0] != x86asm.RAX || !ok || am.Base != x86asm.RIP || am.Index != 0 {
				continue
			}
			addr := uint64(int64(previousPC) + int64(previous.Len) + am.Disp)
			found[uintptr(pc)] = uintptr(addr)
		}
	}
	return found
}

func armStoredClosurePC(code []byte, off int, start uint64) (uint64, bool) {
	var pc uint64
	pcReg := arm64asm.X0 // Until a PC is loaded, only the object register is tracked.
	for off+4 <= len(code) {
		inst, pos, ok := armInstruction(code, off, 4)
		if !ok {
			break
		}
		off = pos + 4
		if pcReg == arm64asm.X0 && inst.Op == arm64asm.ADRP {
			next, nextPos, ok := armInstruction(code, off, 4)
			if ok {
				addr, reg, ok := armAddress(inst, next, start+uint64(pos))
				if ok && reg > arm64asm.X0 && reg <= arm64asm.X30 {
					pc, pcReg, off = addr, reg, nextPos+4
					continue
				}
			}
		}
		if pcReg != arm64asm.X0 {
			var mem arm64asm.MemImmediate
			var offset uint32
			var store bool
			switch inst.Op {
			case arm64asm.STR:
				mem, store = inst.Args[1].(arm64asm.MemImmediate)
				offset = (inst.Enc >> 10) & 0xfff
			case arm64asm.STP:
				mem, store = inst.Args[2].(arm64asm.MemImmediate)
				offset = (inst.Enc >> 15) & 0x7f
			}
			if store && inst.Args[0] == pcReg && mem.Base == arm64asm.RegSP(arm64asm.X0) && mem.Mode == arm64asm.AddrOffset && offset == 0 {
				return pc, true
			}
		}
		if !armPreservesRegisters(inst, pcReg) {
			break
		}
	}
	return 0, false
}

// Only known register effects are allowed on the allocation's fallthrough path.
// Calls, branches, spills and unknown operations end the candidate. For example,
// AND X3,X3,#mask can prepare a capture without changing X0 or a PC in X4.
func armPreservesRegisters(inst arm64asm.Inst, pcReg arm64asm.Reg) bool {
	var memory arm64asm.Arg
	switch inst.Op {
	case arm64asm.NOP, arm64asm.CMP, arm64asm.CMN, arm64asm.TST:
		return true
	case arm64asm.ADD, arm64asm.ADDS, arm64asm.SUB, arm64asm.SUBS,
		arm64asm.AND, arm64asm.ANDS, arm64asm.ORR, arm64asm.EOR,
		arm64asm.LSL, arm64asm.LSR, arm64asm.ASR, arm64asm.NEG,
		arm64asm.MOV, arm64asm.MOVK, arm64asm.MOVZ, arm64asm.MOVN,
		arm64asm.ADR, arm64asm.ADRP:
	case arm64asm.LDR, arm64asm.LDRB, arm64asm.LDRH,
		arm64asm.LDRSB, arm64asm.LDRSH, arm64asm.LDRSW, arm64asm.LDUR:
		memory = inst.Args[1]
	case arm64asm.LDP:
		if !armPreservesRegister(inst.Args[1], pcReg) {
			return false
		}
		memory = inst.Args[2]
	default:
		return false
	}
	if !armPreservesRegister(inst.Args[0], pcReg) {
		return false
	}
	if memory != nil {
		switch mem := memory.(type) {
		case arm64asm.MemImmediate:
			// Pre/post-indexed loads also write their base register.
			return mem.Mode == arm64asm.AddrOffset || armPreservesRegister(mem.Base, pcReg)
		case arm64asm.MemExtend:
			return true
		default:
			return false
		}
	}
	return true
}

func armPreservesRegister(arg arm64asm.Arg, pcReg arm64asm.Reg) bool {
	reg, ok := armReg(arg)
	if !ok {
		return false
	}
	if reg >= arm64asm.W0 && reg <= arm64asm.W30 {
		reg += arm64asm.X0 - arm64asm.W0
	}
	return reg != arm64asm.X0 && reg != pcReg
}

func x86StoredClosurePC(code []byte, off int, start uint64) (uint64, bool) {
	var pc uint64
	var pcReg x86asm.Reg
	for off < len(code) {
		inst, pos, ok := x86Instruction(code, off)
		if !ok {
			break
		}
		off = pos + inst.Len
		if pcReg == 0 && inst.Op == x86asm.LEA && inst.AddrSize == 64 {
			reg, _ := inst.Args[0].(x86asm.Reg)
			mem, ok := inst.Args[1].(x86asm.Mem)
			if ok && reg > x86asm.RAX && reg <= x86asm.R15 && mem.Base == x86asm.RIP && mem.Index == 0 {
				pc, pcReg = uint64(int64(start+uint64(off))+mem.Disp), reg
				continue
			}
		}
		if pcReg != 0 && inst.Op == x86asm.MOV && inst.MemBytes == 8 && inst.AddrSize == 64 {
			mem, ok := inst.Args[0].(x86asm.Mem)
			if ok && mem.Segment == 0 && mem.Base == x86asm.RAX && mem.Index == 0 && mem.Disp == 0 && inst.Args[1] == pcReg {
				return pc, true
			}
		}
		if !x86PreservesRegisters(inst, pcReg) {
			break
		}
	}
	return 0, false
}

func x86PreservesRegisters(inst x86asm.Inst, pcReg x86asm.Reg) bool {
	switch inst.Op {
	case x86asm.NOP, x86asm.CMP, x86asm.TEST:
		return true
	case x86asm.MOV, x86asm.MOVZX, x86asm.MOVSX, x86asm.MOVSXD, x86asm.LEA,
		x86asm.ADD, x86asm.ADC, x86asm.SUB, x86asm.SBB, x86asm.AND, x86asm.OR, x86asm.XOR,
		x86asm.SHL, x86asm.SHR, x86asm.SAR, x86asm.NEG, x86asm.NOT, x86asm.INC, x86asm.DEC:
	case x86asm.IMUL:
		// The one-operand form implicitly writes RAX and RDX.
		if inst.Args[1] == nil {
			return false
		}
	default:
		return false
	}
	reg, ok := inst.Args[0].(x86asm.Reg)
	if !ok {
		return false // Memory writes, including spills, are not followed.
	}
	// Partial writes invalidate the tracked 64-bit value, including AH/CH.
	switch {
	case reg >= x86asm.AL && reg <= x86asm.BL:
		reg = x86asm.RAX + reg - x86asm.AL
	case reg >= x86asm.AH && reg <= x86asm.BH:
		reg = x86asm.RAX + reg - x86asm.AH
	case reg >= x86asm.SPB && reg <= x86asm.R15B:
		reg = x86asm.RSP + reg - x86asm.SPB
	case reg >= x86asm.AX && reg <= x86asm.R15W:
		reg = x86asm.RAX + reg - x86asm.AX
	case reg >= x86asm.EAX && reg <= x86asm.R15L:
		reg = x86asm.RAX + reg - x86asm.EAX
	case reg >= x86asm.RAX && reg <= x86asm.R15:
	default:
		return false
	}
	return reg != x86asm.RAX && reg != pcReg
}

// armInstruction reads the next non-NOP instruction in either direction.
// Unknown instructions end the candidate; they must not join unrelated code.
func armInstruction(code []byte, off, step int) (arm64asm.Inst, int, bool) {
	for off >= 0 && off+4 <= len(code) {
		if binary.LittleEndian.Uint32(code[off:]) == 0xd503201f {
			off += step
			continue
		}
		inst, err := arm64asm.Decode(code[off : off+4])
		return inst, off, err == nil
	}
	return arm64asm.Inst{}, off, false
}

func x86Instruction(code []byte, off int) (x86asm.Inst, int, bool) {
	for off < len(code) {
		inst, err := x86asm.Decode(code[off:], 64)
		if err != nil || inst.Op == 0 || inst.Len == 0 {
			return x86asm.Inst{}, off, false
		}
		if inst.Op != x86asm.NOP {
			return inst, off, true
		}
		off += inst.Len
	}
	return x86asm.Inst{}, off, false
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
