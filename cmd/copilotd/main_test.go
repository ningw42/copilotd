package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/build"
)

func noEnv() func(string) (string, bool) {
	return func(string) (string, bool) { return "", false }
}

func runSuccessfully(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, noEnv(), &stdout, &stderr); code != 0 {
		t.Fatalf("run(%q) exit code = %d, stderr = %q", args, code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(%q) stderr = %q, want empty", args, stderr.String())
	}
	return stdout.String()
}

func TestRunRejectsRemovedVersionFlags(t *testing.T) {
	for _, flag := range []string{"--version", "-version"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{flag}, noEnv(), &stdout, &stderr)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want no build metadata", stdout.String())
			}
			if stderr.Len() == 0 || strings.Contains(stderr.String(), build.String()) {
				t.Errorf("stderr = %q, want a parser error without build metadata", stderr.String())
			}
			if flag == "-version" && (!strings.Contains(stderr.String(), `"-v"`) || strings.Contains(stderr.String(), `"-version"`)) {
				t.Errorf("stderr = %q, want parser-native bundled-short rejection at -v", stderr.String())
			}
		})
	}
}

func TestRunVersionSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, noEnv(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stdout.String() != build.String()+"\n" {
		t.Errorf("stdout = %q, want exactly %q", stdout.String(), build.String()+"\n")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunInformationalCommandsRejectSurplusOperands(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "version", args: []string{"version", "extra"}, want: `copilotd: version: unexpected operand "extra"`},
		{name: "help", args: []string{"help", "serve", "extra"}, want: `copilotd: help: unexpected operand "extra"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, noEnv(), &stdout, &stderr)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
			if strings.Contains(stderr.String(), "USAGE") {
				t.Errorf("stderr = %q, want a concise error without a help dump", stderr.String())
			}
		})
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

func TestRunDispatchAndExplicitHelpAreCaseInsensitive(t *testing.T) {
	t.Run("normal dispatch", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"VeRsIoN"}, noEnv(), &stdout, &stderr)
		if code != 0 || stdout.String() != build.String()+"\n" || stderr.Len() != 0 {
			t.Errorf("run(VeRsIoN) = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
		}
	})

	t.Run("explicit help", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"help", "SeRvE"}, noEnv(), &stdout, &stderr)
		if code != 0 {
			t.Errorf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "copilotd serve [FLAGS]") {
			t.Errorf("stdout = %q, want serve help", stdout.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want empty", stderr.String())
		}
	})
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
	if !strings.Contains(stderr.String(), "run 'copilotd help'") {
		t.Errorf("stderr = %q, want guidance to run copilotd help", stderr.String())
	}
}

func TestRunRejectsOperationalFlagsBeforeSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", "/tmp/copilotd.toml", "serve"}, noEnv(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown") || !strings.Contains(stderr.String(), "config") {
		t.Errorf("stderr = %q, want the parser's unknown-flag error for --config", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunRejectsOperationalFlagsOnInformationalPaths(t *testing.T) {
	flags := []struct {
		name  string
		value string
	}{
		{name: "--log-level", value: "debug"},
		{name: "--log-format", value: "json"},
		{name: "--log-file", value: "/tmp/copilotd.log"},
		{name: "--config", value: "/tmp/copilotd.toml"},
		{name: "--github-oauth-token-file", value: "/tmp/github-oauth-token"},
	}
	paths := []struct {
		name string
		args []string
	}{
		{name: "root"},
		{name: "version", args: []string{"version"}},
		{name: "help", args: []string{"help"}},
	}

	for _, path := range paths {
		for _, flag := range flags {
			t.Run(path.name+"/"+flag.name, func(t *testing.T) {
				args := append(append([]string(nil), path.args...), flag.name, flag.value)
				var stdout, stderr bytes.Buffer
				code := run(args, noEnv(), &stdout, &stderr)
				if code != 1 {
					t.Errorf("exit code = %d, want 1", code)
				}
				if stdout.Len() != 0 {
					t.Errorf("stdout = %q, want empty", stdout.String())
				}
				if !strings.Contains(stderr.String(), "unknown flag") || !strings.Contains(stderr.String(), flag.name) {
					t.Errorf("stderr = %q, want unknown-flag rejection for %s", stderr.String(), flag.name)
				}
			})
		}
	}
}

func TestRunInformationalPathsIgnoreEnvironmentConfiguration(t *testing.T) {
	malformed := filepath.Join(t.TempDir(), "malformed.toml")
	if err := os.WriteFile(malformed, []byte("not valid = ["), 0o600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}

	for _, configPath := range []struct {
		name string
		path string
	}{
		{name: "missing", path: filepath.Join(t.TempDir(), "missing.toml")},
		{name: "malformed", path: malformed},
	} {
		t.Run(configPath.name, func(t *testing.T) {
			env := map[string]string{
				"COPILOTD_CONFIG":    configPath.path,
				"COPILOTD_LOG_LEVEL": "not-a-level",
				"COPILOTD_ADDR":      "not-an-address",
			}
			lookupEnv := func(key string) (string, bool) {
				value, ok := env[key]
				return value, ok
			}

			for _, tc := range []struct {
				name string
				args []string
			}{
				{name: "bare"},
				{name: "help", args: []string{"help"}},
				{name: "help serve", args: []string{"help", "serve"}},
				{name: "version", args: []string{"version"}},
				{name: "root help flag", args: []string{"--help"}},
				{name: "subcommand help flag", args: []string{"serve", "--help"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var stdout, stderr bytes.Buffer
					code := run(tc.args, lookupEnv, &stdout, &stderr)
					if code != 0 {
						t.Errorf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
					}
					if stdout.Len() == 0 || stderr.Len() != 0 {
						t.Errorf("stdout/stderr = %q/%q, want informational output and empty stderr", stdout.String(), stderr.String())
					}
				})
			}
		})
	}
}

func TestRunGeneratedHelpMatchesCommandTree(t *testing.T) {
	bare := runSuccessfully(t)
	explicit := runSuccessfully(t, "help")
	if bare != explicit {
		t.Errorf("bare help and explicit help differ:\n--- bare ---\n%s--- explicit ---\n%s", bare, explicit)
	}
	if !strings.Contains(bare, "copilotd <SUBCOMMAND>") {
		t.Errorf("general help missing exact usage shape:\n%s", bare)
	}
	last := -1
	for _, subcommand := range []string{"version", "help", "serve", "login"} {
		index := strings.Index(bare, "  "+subcommand+" ")
		if index < 0 || index <= last {
			t.Errorf("general help does not list subcommands in version, help, serve, login order:\n%s", bare)
			break
		}
		last = index
	}
	for _, absent := range []string{"--log-level", "--log-format", "--log-file", "--config", "--github-oauth-token-file", "--help", "-h"} {
		if strings.Contains(bare, absent) {
			t.Errorf("general help unexpectedly lists %q:\n%s", absent, bare)
		}
	}

	versionHelp := runSuccessfully(t, "help", "version")
	if !strings.Contains(versionHelp, "copilotd version") {
		t.Errorf("version help does not match the informational contract:\n%s", versionHelp)
	}
	helpHelp := runSuccessfully(t, "help", "help")
	if !strings.Contains(helpHelp, "copilotd help [SUBCOMMAND]") {
		t.Errorf("help help missing exact usage shape:\n%s", helpHelp)
	}
	for _, informationalHelp := range []string{versionHelp, helpHelp} {
		for _, absent := range []string{"--log-level", "--log-format", "--log-file", "--config", "--github-oauth-token-file", "--help", "-h"} {
			if strings.Contains(informationalHelp, absent) {
				t.Errorf("informational help unexpectedly lists %q:\n%s", absent, informationalHelp)
			}
		}
	}

	for _, command := range []string{"serve", "login"} {
		help := runSuccessfully(t, "help", command)
		if !strings.Contains(help, "copilotd "+command+" [FLAGS]") {
			t.Errorf("%s help missing exact usage shape:\n%s", command, help)
		}
		lastShared := -1
		for _, shared := range []string{"--log-level", "--log-format", "--log-file", "--config", "--github-oauth-token-file"} {
			index := strings.Index(help, shared)
			if index < 0 {
				t.Errorf("%s help missing shared flag %s:\n%s", command, shared, help)
			}
			if index <= lastShared {
				t.Errorf("%s help does not order shared flags as specified:\n%s", command, help)
			}
			lastShared = index
		}
		for _, absent := range []string{"--help", "-h"} {
			if strings.Contains(help, "  "+absent+" ") || strings.Contains(help, "  "+absent+"\n") {
				t.Errorf("%s help unexpectedly lists parser-native help flag %q:\n%s", command, absent, help)
			}
		}
		firstSpecific := "--addr"
		if command == "login" {
			firstSpecific = "--github-client-id"
		}
		if strings.Index(help, "--github-oauth-token-file") >= strings.Index(help, firstSpecific) {
			t.Errorf("%s help does not put shared flags before command-specific flags:\n%s", command, help)
		}

		native := runSuccessfully(t, command, "--help")
		if native != help {
			t.Errorf("%s --help differs from help %s:\n--- native ---\n%s--- explicit ---\n%s", command, command, native, help)
		}
	}
}

func TestRunParserNativeHelpFlagsRenderTargetHelp(t *testing.T) {
	general := runSuccessfully(t, "help")
	for _, flag := range []string{"-h", "--help"} {
		if got := runSuccessfully(t, flag); got != general {
			t.Errorf("copilotd %s differs from general help:\n%s", flag, got)
		}
	}
	for _, command := range []string{"version", "help", "serve", "login"} {
		want := runSuccessfully(t, "help", command)
		for _, flag := range []string{"-h", "--help"} {
			if got := runSuccessfully(t, command, flag); got != want {
				t.Errorf("copilotd %s %s differs from help %s:\n%s", command, flag, command, got)
			}
		}
	}
}

func TestRunHelpFlagsDoNotBypassOperandValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "version", args: []string{"version", "--help", "extra"}, want: `copilotd: version: unexpected operand "extra"`},
		{name: "serve", args: []string{"serve", "--help", "extra"}, want: `copilotd: serve: unexpected operand "extra"`},
		{name: "login", args: []string{"login", "-h", "extra"}, want: `copilotd: login: unexpected operand "extra"`},
		{name: "help", args: []string{"help", "--help", "serve", "extra"}, want: `copilotd: help: unexpected operand "extra"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, noEnv(), &stdout, &stderr)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.want) || strings.Contains(stderr.String(), "USAGE") {
				t.Errorf("stderr = %q, want concise operand error containing %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestRunHelpFlagsDoNotHideTrailingInvalidFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		flag string
	}{
		{name: "removed version flag on help", args: []string{"help", "--help", "--version"}, flag: "--version"},
		{name: "removed bundled version flag on help", args: []string{"help", "--help", "-version"}, flag: "-v"},
		{name: "removed version flag on version", args: []string{"version", "--help", "--version"}, flag: "--version"},
		{name: "unknown serve flag", args: []string{"serve", "--help", "--bogus"}, flag: "--bogus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, noEnv(), &stdout, &stderr)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), "unknown flag") || !strings.Contains(stderr.String(), tc.flag) {
				t.Errorf("stderr = %q, want parser unknown-flag error for %s", stderr.String(), tc.flag)
			}
			if tc.flag == "-v" && strings.Contains(stderr.String(), `"-version"`) {
				t.Errorf("stderr = %q, want parser-native bundled-short rejection at -v", stderr.String())
			}
		})
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"serve", "--help", "--log-level", "debug"}, noEnv(), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "copilotd serve [FLAGS]") || stderr.Len() != 0 {
		t.Errorf("valid flag after native help = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}

	wantHelpCommand := runSuccessfully(t, "help", "help")
	gotHelpCommand := runSuccessfully(t, "help", "--help", "serve")
	if gotHelpCommand != wantHelpCommand {
		t.Errorf("help --help with one valid operand differs from help command help:\n%s", gotHelpCommand)
	}
}

func TestRunParserErrorsHaveCommandQualification(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--bogus"}, noEnv(), &stdout, &stderr)
		if code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
		if count := strings.Count(stderr.String(), "copilotd:"); count != 1 {
			t.Errorf("stderr = %q, want exactly one copilotd: prefix; got %d", stderr.String(), count)
		}
		if !strings.HasPrefix(stderr.String(), "copilotd:") || !strings.Contains(stderr.String(), "bogus") {
			t.Errorf("stderr = %q, want a qualified root parser error", stderr.String())
		}
	})

	t.Run("subcommand", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"serve", "--bogus"}, noEnv(), &stdout, &stderr)
		if code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
		if !strings.HasPrefix(stderr.String(), "copilotd: serve:") || !strings.Contains(stderr.String(), "bogus") {
			t.Errorf("stderr = %q, want a copilotd: serve: parser error", stderr.String())
		}
		if strings.Contains(stderr.String(), "USAGE") || stdout.Len() != 0 {
			t.Errorf("stdout/stderr = %q/%q, want a concise error without help", stdout.String(), stderr.String())
		}
	})
}

func TestRunOperationalCommandsRejectOperandsBeforeConfiguration(t *testing.T) {
	for _, command := range []string{"serve", "login"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			githubOAuthTokenFile := filepath.Join(dir, "github-oauth-token")
			logFile := filepath.Join(dir, "copilotd.log")
			env := map[string]string{
				"COPILOTD_CONFIG":                  filepath.Join(dir, "missing.toml"),
				"COPILOTD_GITHUB_OAUTH_TOKEN_FILE": githubOAuthTokenFile,
				"COPILOTD_LOG_FILE":                logFile,
			}
			lookupEnv := func(key string) (string, bool) {
				value, ok := env[key]
				return value, ok
			}

			args := []string{
				command,
				"--log-level", "debug",
				"--log-format", "json",
				"--log-file", logFile,
				"--config", filepath.Join(dir, "missing.toml"),
				"--github-oauth-token-file", githubOAuthTokenFile,
				"unexpected",
			}
			var stdout, stderr bytes.Buffer
			code := run(args, lookupEnv, &stdout, &stderr)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			want := `copilotd: ` + command + `: unexpected operand "unexpected"`
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
			}
			if strings.Contains(stderr.String(), "config file") || strings.Contains(stderr.String(), "USAGE") {
				t.Errorf("stderr = %q, want operand precedence and no help dump", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			for _, path := range []string{githubOAuthTokenFile, logFile} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("side-effect path %q exists (stat error %v), want no command side effects", path, err)
				}
			}
		})
	}
}
