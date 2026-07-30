//go:build unix

package mcp

import "syscall"

func createLocalCopyFIFO(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
