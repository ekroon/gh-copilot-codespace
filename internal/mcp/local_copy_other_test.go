//go:build !unix

package mcp

import "errors"

func createLocalCopyFIFO(string) error {
	return errors.New("FIFO unsupported")
}
