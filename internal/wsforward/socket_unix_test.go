//go:build unix

package wsforward

import "syscall"

const slowPeerSendBuffer = 16 << 10

func slowPeerSetReceiveBuffer(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 64<<10)
}

func slowPeerGetSocketBuffer(fd uintptr, option int) (int, error) {
	return syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, option)
}
