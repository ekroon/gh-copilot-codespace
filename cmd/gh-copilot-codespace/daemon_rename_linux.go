package main

import "golang.org/x/sys/unix"

func daemonRenameNoReplace(source, target string) error {
	return daemonRenameAtNoReplace(unix.AT_FDCWD, source, target)
}

func daemonRenameExchange(source, target string) error {
	return daemonRenameAtExchange(unix.AT_FDCWD, source, target)
}

func daemonRenameAtNoReplace(parentFD int, source, target string) error {
	return unix.Renameat2(parentFD, source, parentFD, target, unix.RENAME_NOREPLACE)
}

func daemonRenameAtExchange(parentFD int, source, target string) error {
	return unix.Renameat2(parentFD, source, parentFD, target, unix.RENAME_EXCHANGE)
}
