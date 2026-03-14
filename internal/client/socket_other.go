//go:build !linux

package client

import "syscall"

func bindSocketToDevice(network, address, ifaceName string, c syscall.RawConn) error {
	_ = network
	_ = address
	_ = ifaceName
	_ = c
	return nil
}
