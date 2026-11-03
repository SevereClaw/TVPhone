package main

import (
	"net"
	"syscall"
)

func setMulticastIf(c *net.UDPConn, ip net.IP) {
	rc, err := c.SyscallConn()
	if err != nil {
		return
	}
	var a [4]byte
	copy(a[:], ip.To4())
	rc.Control(func(fd uintptr) {
		syscall.SetsockoptInet4Addr(syscall.Handle(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, a)
	})
}
