package wsforward

import (
	"context"
	"errors"
	"io"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/shim"
	"golang.org/x/sync/errgroup"
)

type sessionPeer int

const (
	clientPeer sessionPeer = iota
	upstreamPeer
)

type pumpOperation int

const (
	readOperation pumpOperation = iota
	writeOperation
	transformOperation
)

type pumpStats struct {
	messages int64
	bytes    int64
}

type pumpFailure struct {
	peer      sessionPeer
	operation pumpOperation
	err       error
}

type sessionResult struct {
	messagesClientToUpstream int64
	messagesUpstreamToClient int64
	bytesClientToUpstream    int64
	bytesUpstreamToClient    int64
	closeCode                websocket.StatusCode
	terminal                 SessionTerminal
}

// runSession pumps messages in both directions until either peer fails. Pump
// goroutines rendezvous here before returning so the terminal close frames are
// sent before errgroup cancellation tears down the sibling connection.
func runSession(drainCtx, baseCtx context.Context, client, upstream *websocket.Conn, writeTimeout time.Duration, maxMessageBytes int64, clientToUpstreamTransform, upstreamToClientTransform shim.MessageTransform) sessionResult {
	client.SetReadLimit(maxMessageBytes)
	upstream.SetReadLimit(maxMessageBytes)

	sessionCtx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	group, pumpCtx := errgroup.WithContext(sessionCtx)
	terminal := make(chan pumpFailure, 2)
	release := make(chan struct{})
	var clientToUpstream, upstreamToClient pumpStats
	startPump := func(source, destination *websocket.Conn, sourcePeer, destinationPeer sessionPeer, stats *pumpStats, transform shim.MessageTransform) {
		group.Go(func() error {
			observed, failure := pump(pumpCtx, source, destination, sourcePeer, destinationPeer, writeTimeout, transform)
			*stats = observed
			terminal <- failure
			<-release
			return failure.err
		})
	}
	startPump(client, upstream, clientPeer, upstreamPeer, &clientToUpstream, clientToUpstreamTransform)
	startPump(upstream, client, upstreamPeer, clientPeer, &upstreamToClient, upstreamToClientTransform)

	var failure pumpFailure
	shutdown := false
	select {
	case failure = <-terminal:
	case <-drainCtx.Done():
		shutdown = true
	}
	if baseCtx.Err() != nil {
		close(release)
		cancel()
		_ = group.Wait()
		return resultFrom(clientToUpstream, upstreamToClient, websocket.StatusAbnormalClosure, SessionError)
	}
	code, reason := terminalClose(failure, shutdown)
	closeBoth(client, upstream, code, reason)
	close(release)
	cancel()
	_ = group.Wait()
	return resultFrom(clientToUpstream, upstreamToClient, code, terminalReason(failure, code, shutdown))
}

func resultFrom(clientToUpstream, upstreamToClient pumpStats, code websocket.StatusCode, terminal SessionTerminal) sessionResult {
	return sessionResult{
		messagesClientToUpstream: clientToUpstream.messages,
		messagesUpstreamToClient: upstreamToClient.messages,
		bytesClientToUpstream:    clientToUpstream.bytes,
		bytesUpstreamToClient:    upstreamToClient.bytes,
		closeCode:                code,
		terminal:                 terminal,
	}
}

func pump(ctx context.Context, source, destination *websocket.Conn, sourcePeer, destinationPeer sessionPeer, writeTimeout time.Duration, transform shim.MessageTransform) (pumpStats, pumpFailure) {
	var stats pumpStats
	for {
		messageType, payload, err := source.Read(ctx)
		if err != nil {
			return stats, pumpFailure{peer: sourcePeer, operation: readOperation, err: err}
		}
		if transform != nil {
			message := shim.Message{Kind: kindFromType(messageType), Data: payload}
			emit, transformErr := transform(ctx, &message)
			if transformErr != nil {
				return stats, pumpFailure{peer: sourcePeer, operation: transformOperation, err: transformErr}
			}
			if !emit {
				continue
			}
			messageType, payload = typeFromKind(message.Kind), message.Data
		}

		writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
		err = destination.Write(writeCtx, messageType, payload)
		writeCtxErr := writeCtx.Err()
		cancel()
		if err != nil {
			if errors.Is(writeCtxErr, context.DeadlineExceeded) {
				err = context.DeadlineExceeded
			}
			return stats, pumpFailure{peer: destinationPeer, operation: writeOperation, err: err}
		}
		stats.messages++
		stats.bytes += int64(len(payload))
	}
}

func kindFromType(messageType websocket.MessageType) shim.MessageKind {
	if messageType == websocket.MessageBinary {
		return shim.MessageBinary
	}
	return shim.MessageText
}

func typeFromKind(kind shim.MessageKind) websocket.MessageType {
	if kind == shim.MessageBinary {
		return websocket.MessageBinary
	}
	return websocket.MessageText
}

func terminalClose(failure pumpFailure, shutdown bool) (websocket.StatusCode, string) {
	if shutdown {
		return websocket.StatusGoingAway, "going away"
	}
	if errors.Is(failure.err, websocket.ErrMessageTooBig) {
		return websocket.StatusMessageTooBig, "message too big"
	}
	if isAbruptClientDisconnect(failure) {
		return websocket.StatusGoingAway, "client disconnected"
	}
	if code := websocket.CloseStatus(failure.err); isSendableCloseCode(code) {
		var closeErr websocket.CloseError
		if errors.As(failure.err, &closeErr) {
			return code, closeErr.Reason
		}
		return code, ""
	}
	return websocket.StatusInternalError, "internal error"
}

func isAbruptClientDisconnect(failure pumpFailure) bool {
	if failure.peer != clientPeer || failure.operation != readOperation || websocket.CloseStatus(failure.err) != -1 {
		return false
	}
	return errors.Is(failure.err, io.EOF) ||
		errors.Is(failure.err, syscall.ECONNRESET) ||
		errors.Is(failure.err, syscall.EPIPE)
}

func terminalReason(failure pumpFailure, code websocket.StatusCode, shutdown bool) SessionTerminal {
	if shutdown {
		return SessionUpstreamClosed
	}
	if failure.operation == writeOperation && !isSendableCloseCode(websocket.CloseStatus(failure.err)) {
		return SessionError
	}
	if abnormalCloseCode(code) {
		return SessionError
	}
	if failure.peer == clientPeer {
		return SessionClientClosed
	}
	return SessionUpstreamClosed
}

func abnormalCloseCode(code websocket.StatusCode) bool {
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		return false
	}
	// Application-defined close codes carry application lifecycle semantics;
	// absent evidence of a transport failure, classify them by the closing peer.
	if code >= 3000 && code <= 4999 {
		return false
	}
	return true
}

func isSendableCloseCode(code websocket.StatusCode) bool {
	return code != -1 &&
		code != websocket.StatusNoStatusRcvd &&
		code != websocket.StatusAbnormalClosure &&
		code != websocket.StatusTLSHandshake
}

func closeBoth(client, upstream *websocket.Conn, code websocket.StatusCode, reason string) {
	var wg sync.WaitGroup
	wg.Add(2)
	for _, conn := range []*websocket.Conn{client, upstream} {
		go func() {
			defer wg.Done()
			_ = conn.Close(code, reason)
		}()
	}
	wg.Wait()
}
