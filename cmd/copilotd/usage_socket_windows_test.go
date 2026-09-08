package main

import "syscall"

// Winsock documents SO_SNDBUF=0 as disabling send buffering, including for
// overlapped I/O. A positive value is not a hard quota for a large WSASend.
const usageBlockedSendBuffer = 0

func usageSetReceiveBuffer(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 64<<10)
}

func usageGetSocketBuffer(fd uintptr, option int) (int, error) {
	return syscall.GetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, option)
}
