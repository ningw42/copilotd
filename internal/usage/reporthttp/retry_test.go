package reporthttp

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
)

func TestClientDoesNotRetryRefusedHTTPRequests(t *testing.T) {
	certificates := httptest.NewTLSServer(http.NotFoundHandler())
	defer certificates.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certificates.TLS.Certificates, NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var workers sync.WaitGroup
	var connections sync.Map
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(conn, true)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				defer connections.Delete(conn)
				serveRefusedReport(conn, &requests)
			}()
		}
	}()
	defer func() {
		_ = listener.Close()
		connections.Range(func(key, value any) bool { _ = key.(net.Conn).Close(); return true })
		workers.Wait()
	}()
	client, err := NewClient("https://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// Private TLS trust fixture, not disabled certificate verification. The
	// observation is one real Client.Query against a refusing network peer.
	roots := x509.NewCertPool()
	roots.AddCert(certificates.Certificate())
	client.http.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, err = client.Query(ctx, report.Query{Timezone: "UTC"})
	if err == nil || requests.Load() != 1 {
		t.Fatalf("one request expected: observed=%d err=%v", requests.Load(), err)
	}
}

func serveRefusedReport(conn net.Conn, requests *atomic.Int32) {
	secure := conn.(*tls.Conn)
	if secure.Handshake() != nil {
		return
	}
	if secure.ConnectionState().NegotiatedProtocol != "h2" {
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		requests.Add(1)
		_, _ = io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	preface := make([]byte, 24)
	if _, err := io.ReadFull(conn, preface); err != nil {
		return
	}
	// HTTP/2 SETTINGS, then REFUSED_STREAM for each request HEADERS. Unlike an
	// application error, this transport signal can trigger transparent retries.
	_, _ = conn.Write([]byte{0, 0, 0, 4, 0, 0, 0, 0, 0})
	for {
		var header [9]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		size := int64(header[0])<<16 | int64(header[1])<<8 | int64(header[2])
		if _, err := io.CopyN(io.Discard, conn, size); err != nil {
			return
		}
		if header[3] == 4 && header[4]&1 == 0 {
			_, _ = conn.Write([]byte{0, 0, 0, 4, 1, 0, 0, 0, 0})
		}
		if header[3] == 1 {
			requests.Add(1)
			frame := []byte{0, 0, 4, 3, 0, 0, 0, 0, 0, 0, 0, 0, 7}
			binary.BigEndian.PutUint32(frame[5:9], binary.BigEndian.Uint32(header[5:9]))
			if _, err := conn.Write(frame); err != nil {
				return
			}
		}
	}
}
