//go:build linux

package mcp

import "golang.org/x/sys/unix"

func renameLocalCopyNoReplace(parentFD int, source, target string) error {
	return unix.Renameat2(parentFD, source, parentFD, target, unix.RENAME_NOREPLACE)
}

func exchangeLocalCopy(parentFD int, source, target string) error {
	return unix.Renameat2(parentFD, source, parentFD, target, unix.RENAME_EXCHANGE)
}
