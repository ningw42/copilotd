package wsforward

import "syscall"

// Disable Winsock send buffering for this motionless downstream only. Positive
// SO_SNDBUF values do not impose a hard quota on a large overlapped WSASend.
const slowPeerSendBuffer = 0

func slowPeerSetReceiveBuffer(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 64<<10)
}

func slowPeerGetSocketBuffer(fd uintptr, option int) (int, error) {
	return syscall.GetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, option)
}
