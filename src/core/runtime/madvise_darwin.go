//go:build darwin && (amd64 || arm64) && !tinygo

package runtime

import (
	"runtime"
	"syscall"
	"unsafe"
)

const madvZero = 11

func madviseDontNeed(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	// MADV_ZERO reports success under Rosetta but leaves Intel pages intact.
	// Clear there to preserve the allocator's zero-on-reuse contract. Native
	// Apple Silicon uses the lazy kernel operation below.
	if runtime.GOARCH == "amd64" {
		clear(b)
		return nil
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_MADVISE,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), madvZero); errno == 0 {
		return nil
	}
	// MADV_ZERO can reject fork-created copy-on-write mappings. Darwin's
	// MADV_DONTNEED does not guarantee zeroed pages, so clear on failure.
	clear(b)
	return nil
}
