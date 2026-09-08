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
	for _, args := range [][]string{nil, {"version"}, {"help", "usage"}, {"usage", "--help"}, {"serve", "--help"}, {"login", "--help"}, {"--help", "usage"}, {"--help", "usage", "--help"}, {"UsAgE", "-h"}} {
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

func TestUsageExecutableMalformedFlagsWithClosedStderrPipe(t *testing.T) {
	binary := usageAcceptanceBinary(t)
	for _, args := range [][]string{
		{"usage", "--unknown"},
		{"UsAgE", "--unknown"},
		{"--", "usage", "--unknown"},
		{"usage", "--timeout", "invalid"},
		{"usage", "--model"},
		{"usage", "extra"},
	} {
		usageExecClosedStderr(t, binary, args...)
	}
}

func TestUsageExecutableMalformedHelpWithClosedStderrPipe(t *testing.T) {
	binary := usageAcceptanceBinary(t)
	for _, args := range [][]string{
		{"usage", "--help", "--unknown"},
		{"usage", "--help", "extra"},
		{"usage", "-h", "--timeout", "invalid"},
		{"usage", "--help", "--", "--unknown"},
		{"--help", "usage", "--unknown"},
		{"--help", "usage", "extra"},
		{"--help", "UsAgE", "--help", "-h", "--unknown"},
	} {
		usageExecClosedStderr(t, binary, args...)
	}
}

func usageExecClosedStderr(t *testing.T, binary string, args ...string) {
	t.Helper()
	// An ordinary stderr must still carry the existing diagnostic with no help
	// or successful output; losing that diagnostic to a pipe changes only its
	// delivery, not the command's exit status.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	control := exec.CommandContext(ctx, binary, args...)
	control.Env = usageExecEnv(nil)
	var controlOut, controlErr bytes.Buffer
	control.Stdout, control.Stderr = &controlOut, &controlErr
	controlResult := control.Run()
	cancel()
	controlExit, ok := controlResult.(*exec.ExitError)
	if !ok || controlExit.ExitCode() != 1 || controlOut.Len() != 0 || !strings.HasPrefix(controlErr.String(), "copilotd:") {
		t.Fatalf("argv=%q: normal stderr control: result=%v stdout=%q stderr=%q", args, controlResult, controlOut.String(), controlErr.String())
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writeEnd.Close()
	if err := readEnd.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = usageExecEnv(nil)
	var stdout bytes.Buffer
	command.Stdout, command.Stderr = &stdout, writeEnd
	err = command.Run()
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("argv=%q: expected exit 1, got %v", args, err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	t.Logf("argv=%q: result=%v status=%v stdout=%q", args, err, status, stdout.String())
	if !ok || status.Signaled() || exit.ExitCode() != 1 || stdout.Len() != 0 {
		t.Fatalf("argv=%q: malformed usage must exit 1 even when stderr delivery fails: %v", args, err)
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
