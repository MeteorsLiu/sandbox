//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"debug/elf"
	"fmt"
	"os"
	"runtime"
)

func guestMain() (uintptr, error) {
	path, err := os.Executable()
	if err != nil {
		return 0, err
	}
	f, err := elf.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if f.Type != elf.ET_EXEC {
		return 0, fmt.Errorf("sandbox requires a non-PIE ET_EXEC executable, got %s", f.Type)
	}
	syms, err := f.Symbols()
	if err != nil {
		return 0, fmt.Errorf("sandbox needs ELF symbols: %w", err)
	}
	jumpSize := uint64(4)
	if runtime.GOARCH == "amd64" {
		jumpSize = 5
	}
	for _, symbol := range syms {
		if symbol.Name == "main.main" && elf.ST_TYPE(symbol.Info) == elf.STT_FUNC && symbol.Size >= jumpSize {
			return uintptr(symbol.Value), nil
		}
	}
	return 0, fmt.Errorf("sandbox needs a main.main ELF symbol with at least %d bytes", jumpSize)
}
