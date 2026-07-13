package main

import (
	"bytes"
	"strings"
	"testing"
)

func noEnv() func(string) (string, bool) {
	return func(string) (string, bool) { return "", false }
}

func TestRunVersionShortCircuits(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// A deliberately invalid --addr proves --version returns before Load runs.
	code := run([]string{"--version", "--addr", "not-valid"}, noEnv(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Errorf("expected build info on stdout, got empty")
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr, got %q", stderr.String())
	}
}

func TestRunConfigErrorExitsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--addr", "127.0.0.1"}, noEnv(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "addr") {
		t.Errorf("stderr = %q, want it to explain the bad addr", stderr.String())
	}
}
