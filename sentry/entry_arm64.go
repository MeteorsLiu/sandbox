package main

import (
	"encoding/binary"
	"fmt"
)

func entryJump(from, to uintptr) ([]byte, error) {
	if from == 0 || to == 0 || from%4 != 0 || to%4 != 0 {
		return nil, fmt.Errorf("guest main and entry addresses must be nonzero and aligned to 4 bytes")
	}
	delta := int64(to) - int64(from)
	if delta < -1<<27 || delta >= 1<<27 {
		return nil, fmt.Errorf("guest entry is outside the arm64 branch range")
	}
	jump := make([]byte, 4)
	binary.LittleEndian.PutUint32(jump, 0x14000000|uint32(delta>>2)&0x03ffffff)
	return jump, nil
}
