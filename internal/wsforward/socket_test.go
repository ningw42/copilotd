package wsforward

import (
	"fmt"
	"net"
	"syscall"
	"testing"
)

func slowPeerControl(t *testing.T) func(string, string, syscall.RawConn) error {
	return func(_, address string, raw syscall.RawConn) error {
		var optionErr error
		if err := raw.Control(func(fd uintptr) { optionErr = slowPeerSetReceiveBuffer(fd) }); err != nil {
			return err
		}
		if optionErr != nil {
			return optionErr
		}
		// Set the receive window before connect; shrinking after negotiation
		// does not establish a slow receiver on Windows. Keep 65536 for scaling.
		return slowPeerLogRawBuffers(t, "pre-connect downstream requested SO_RCVBUF=65536 remote="+address, raw)
	}
}

// This is a real accepted socket, configured before its first HTTP/WS write.
// Only the motionless downstream fixture uses it; the upstream is not throttled.
type slowReaderListener struct {
	net.Listener
	t *testing.T
}

func (l slowReaderListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tcp := conn.(*net.TCPConn)
	if err := tcp.SetWriteBuffer(slowPeerSendBuffer); err != nil {
		_ = conn.Close()
		return nil, err
	}
	raw, err := tcp.SyscallConn()
	if err == nil {
		err = slowPeerLogRawBuffers(l.t, fmt.Sprintf("blocked server requested SO_SNDBUF=%d local=%s remote=%s", slowPeerSendBuffer, tcp.LocalAddr(), tcp.RemoteAddr()), raw)
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func slowPeerLogRawBuffers(t *testing.T, phase string, raw syscall.RawConn) error {
	var receive, send int
	var optionErr error
	if err := raw.Control(func(fd uintptr) {
		receive, optionErr = slowPeerGetSocketBuffer(fd, syscall.SO_RCVBUF)
		if optionErr == nil {
			send, optionErr = slowPeerGetSocketBuffer(fd, syscall.SO_SNDBUF)
		}
	}); err != nil {
		return err
	}
	if optionErr != nil {
		return optionErr
	}
	t.Logf("TCP fixture %s: SO_RCVBUF=%d SO_SNDBUF=%d (OS readback, not a hard quota)", phase, receive, send)
	return nil
}
