package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/build"
)

func noEnv() func(string) (string, bool) {
	return func(string) (string, bool) { return "", false }
}

func TestRunVersionFlagShortCircuits(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// A deliberately invalid --addr proves --version returns before config loads.
	code := run([]string{"--version", "--addr", "not-valid"}, noEnv(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), build.String()) {
		t.Errorf("stdout = %q, want it to contain build info %q", stdout.String(), build.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr, got %q", stderr.String())
	}
}

func TestRunVersionSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, noEnv(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), build.String()) {
		t.Errorf("stdout = %q, want it to contain build info %q", stdout.String(), build.String())
	}
}

func TestRunServeConfigErrorExitsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"serve", "--addr", "not-valid"}, noEnv(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "addr") {
		t.Errorf("stderr = %q, want it to explain the bad addr", stderr.String())
	}
}

func TestRunBarePrintsGeneralHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, noEnv(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	for _, sub := range []string{"serve", "login", "version", "help"} {
		if !strings.Contains(out, sub) {
			t.Errorf("general help missing subcommand %q:\n%s", sub, out)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr, got %q", stderr.String())
	}
}

func TestRunHelpPrintsGeneralHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"help"}, noEnv(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "serve") {
		t.Errorf("`help` should print general help listing subcommands:\n%s", stdout.String())
	}
}

func TestRunHelpSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"help", "serve"}, noEnv(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	// serve's help must show a serve-specific flag.
	if !strings.Contains(stdout.String(), "addr") {
		t.Errorf("`help serve` should render serve's help (with --addr):\n%s", stdout.String())
	}
}

func TestRunHelpLoginSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"help", "login"}, noEnv(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	// login's help must render its login-specific flags.
	out := stdout.String()
	for _, want := range []string{"github-client-id", "github-scope"} {
		if !strings.Contains(out, want) {
			t.Errorf("`help login` should render login's help (with --%s):\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr, got %q", stderr.String())
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, noEnv(), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit code = %d, want non-zero for an unknown subcommand", code)
	}
	if !strings.Contains(stderr.String(), "bogus") {
		t.Errorf("stderr = %q, want it to name the unknown subcommand", stderr.String())
	}
}
