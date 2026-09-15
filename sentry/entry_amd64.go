package main

import (
	"encoding/binary"
	"fmt"
)

func entryJump(from, to uintptr) ([]byte, error) {
	if from == 0 || to == 0 {
		return nil, fmt.Errorf("guest main and entry addresses are required")
	}
	delta := int64(to) - int64(from) - 5
	if delta < -1<<31 || delta >= 1<<31 {
		return nil, fmt.Errorf("guest entry is outside the amd64 relative jump range")
	}
	jump := []byte{0xe9, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(jump[1:], uint32(int32(delta)))
	return jump, nil
}
