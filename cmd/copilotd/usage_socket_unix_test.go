//go:build unix

package main

import "syscall"

const usageBlockedSendBuffer = 16 << 10

func usageSetReceiveBuffer(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 64<<10)
}

func usageGetSocketBuffer(fd uintptr, option int) (int, error) {
	return syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, option)
}
