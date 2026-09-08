//go:build unix

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUsageExecutableInformationalCommandsRetainSIGPIPE(t *testing.T) {
	binary := usageAcceptanceBinary(t)
	for _, args := range [][]string{nil, {"version"}, {"help", "usage"}, {"usage", "--help"}, {"serve", "--help"}, {"login", "--help"}} {
		readEnd, writeEnd, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := readEnd.Close(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		command := exec.CommandContext(ctx, binary, args...)
		command.Env = usageExecEnv(nil)
		command.Stdout = writeEnd
		var stderr bytes.Buffer
		command.Stderr = &stderr
		err = command.Run()
		cancel()
		_ = writeEnd.Close()
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("argv=%q: expected unchanged Unix SIGPIPE behavior, got %v", args, err)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGPIPE || stderr.Len() != 0 {
			t.Fatalf("argv=%q: informational command signal policy changed: %v stderr=%q", args, err, stderr.String())
		}
	}
}

func TestUsageExecutableCancellationUsesCLIErrorPath(t *testing.T) {
	binary := usageAcceptanceBinary(t)
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			entered := make(chan struct{})
			edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-r.Context().Done()
			}))
			t.Cleanup(edge.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "usage", "--endpoint", edge.URL, "--timezone", "UTC", "--json")
			command.Env = usageExecEnv(nil)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("usage exited before HTTP request: %v", err)
			}
			if err := command.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			err := <-done
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "copilotd: report request failed:") {
				t.Fatalf("signal did not use canceled command context: result=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
		})
	}
}
