//go:build !windows

package text

import (
	"fmt"
	"os"
	"syscall"
)

// mapFile maps path READ-ONLY on a POSIX host. Same contract as the Windows one: the bytes
// are file-backed and shared between every process that maps them.
func mapFile(path string) ([]byte, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	size := int(fi.Size())
	if size == 0 {
		f.Close()
		return nil, nil, fmt.Errorf("%s is empty", path)
	}
	buf, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	closer := func() {
		syscall.Munmap(buf)
		f.Close()
	}
	return buf, closer, nil
}
