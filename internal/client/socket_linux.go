//go:build linux

package client

import (
	"fmt"
	"syscall"
)

func bindSocketToDevice(network, address, ifaceName string, c syscall.RawConn) error {
	_ = network
	_ = address

	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifaceName)
	}); err != nil {
		return err
	}

	if sockErr != nil {
		return fmt.Errorf("bind to interface %q failed: %w", ifaceName, sockErr)
	}
	return nil
}
