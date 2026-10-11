//go:build !linux

package main

import "net"

func newDashboardListener(address string) (net.Listener, error) {
	return net.Listen("tcp", address)
}
