//go:build darwin

package mcp

import (
	"syscall"
	"unsafe"
)

const (
	localCopyRenameatxNPSyscall = 488
	localCopyRenameExcl         = 0x00000004
	localCopyRenameSwap         = 0x00000002
)

func renameLocalCopyNoReplace(parentFD int, source, target string) error {
	return renameLocalCopyWithFlags(parentFD, source, target, localCopyRenameExcl)
}

func exchangeLocalCopy(parentFD int, source, target string) error {
	return renameLocalCopyWithFlags(parentFD, source, target, localCopyRenameSwap)
}

func renameLocalCopyWithFlags(parentFD int, source, target string, flags uintptr) error {
	sourcePtr, err := syscall.BytePtrFromString(source)
	if err != nil {
		return err
	}
	targetPtr, err := syscall.BytePtrFromString(target)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(
		localCopyRenameatxNPSyscall,
		uintptr(parentFD),
		uintptr(unsafe.Pointer(sourcePtr)),
		uintptr(parentFD),
		uintptr(unsafe.Pointer(targetPtr)),
		flags,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
