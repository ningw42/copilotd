//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func notifyUsagePipeErrors() func() {
	// Notify makes writes to stdout/stderr return EPIPE instead of Go's fatal
	// fd-1/fd-2 SIGPIPE behavior. No receiver goroutine is needed: the write error
	// is authoritative, and signal delivery to this bounded channel never blocks.
	// Stop removes only this registration, restoring ordinary handling when it
	// was the last subscriber; unlike Ignore/Reset it preserves other subscribers.
	pipe := make(chan os.Signal, 1)
	signal.Notify(pipe, syscall.SIGPIPE)
	return func() { signal.Stop(pipe) }
}
