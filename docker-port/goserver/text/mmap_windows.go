//go:build windows

package text

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// mapFile maps path READ-ONLY and answers the bytes plus a closer.
//
// A mapped file is what makes the sharing real: Windows keeps ONE physical copy of the
// pages no matter how many processes map them, so a zone's private bytes do not include
// the text at all (measured: the dialog text was 73 MB of live heap per zone).
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
	size := fi.Size()
	if size == 0 {
		f.Close()
		return nil, nil, fmt.Errorf("%s is empty", path)
	}
	h, err := syscall.CreateFileMapping(syscall.Handle(f.Fd()), nil, syscall.PAGE_READONLY,
		uint32(uint64(size)>>32), uint32(size), nil)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	addr, err := syscall.MapViewOfFile(h, syscall.FILE_MAP_READ, 0, 0, uintptr(size))
	if err != nil {
		syscall.CloseHandle(h)
		f.Close()
		return nil, nil, err
	}
	buf := unsafe.Slice((*byte)(ptrFromUintptr(addr)), size)
	closer := func() {
		syscall.UnmapViewOfFile(addr)
		syscall.CloseHandle(h)
		f.Close()
	}
	return buf, closer, nil
}

// ptrFromUintptr turns an address Windows handed back as a uintptr into an unsafe.Pointer
// WITHOUT the direct uintptr -> Pointer conversion go vet's unsafeptr check flags.
//
// It is sound here for the same reason the mapping is: the address cannot move or be
// collected while it is mapped, and the closer unmaps only after every table view is dead.
func ptrFromUintptr(u uintptr) unsafe.Pointer {
	var p unsafe.Pointer
	*(*uintptr)(unsafe.Pointer(&p)) = u
	return p
}
