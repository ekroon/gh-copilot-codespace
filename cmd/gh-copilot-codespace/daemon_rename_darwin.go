package main

import (
	"syscall"
	"unsafe"
)

const (
	daemonRenameatxNPSyscall = 488
	daemonRenameExcl         = 0x00000004
	daemonRenameSwap         = 0x00000002
)

func daemonRenameNoReplace(source, target string) error {
	return daemonRenameAtNoReplace(-2, source, target)
}

func daemonRenameExchange(source, target string) error {
	return daemonRenameAtExchange(-2, source, target)
}

func daemonRenameAtNoReplace(parentFD int, source, target string) error {
	return daemonRenameAtWithFlags(parentFD, source, target, daemonRenameExcl)
}

func daemonRenameAtExchange(parentFD int, source, target string) error {
	return daemonRenameAtWithFlags(parentFD, source, target, daemonRenameSwap)
}

func daemonRenameAtWithFlags(parentFD int, source, target string, flags uintptr) error {
	sourcePtr, err := syscall.BytePtrFromString(source)
	if err != nil {
		return err
	}
	targetPtr, err := syscall.BytePtrFromString(target)
	if err != nil {
		return err
	}

	_, _, errno := syscall.Syscall6(
		daemonRenameatxNPSyscall,
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
