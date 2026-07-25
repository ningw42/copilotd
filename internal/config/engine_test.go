package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/peterbourgon/ff/v4"
)

func TestResolveAppliesPrecedenceInOrder(t *testing.T) {
	type target struct {
		value string
	}

	path := filepath.Join(t.TempDir(), "copilotd.toml")
	if err := os.WriteFile(path, []byte("value = \"file\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		path string
		env  map[string]string
		args []string
		want string
	}{
		{name: "default", want: "default"},
		{name: "file", path: path, want: "file"},
		{name: "env", path: path, env: map[string]string{"COPILOTD_VALUE": "env"}, want: "env"},
		{name: "flag", path: path, env: map[string]string{"COPILOTD_VALUE": "env"}, args: []string{"--value", "flag"}, want: "flag"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := ff.NewFlagSet("test")
			specs := []spec[target]{
				stringField("value", "default", func(c *target) *string { return &c.value }, nil, "test value"),
			}
			for _, s := range specs {
				s.register(fs)
			}
			if err := ff.Parse(fs, tc.args); err != nil {
				t.Fatalf("parse flags: %v", err)
			}

			lookupEnv := func(key string) (string, bool) {
				value, ok := tc.env[key]
				return value, ok
			}
			var got target
			if err := resolve(specs, fs, &got, tc.path, lookupEnv, nil); err != nil {
				t.Fatalf("resolve() error = %v", err)
			}
			if got.value != tc.want {
				t.Errorf("value = %q, want %q", got.value, tc.want)
			}

			// A registered table is reusable: resolving another target must retain
			// the same parsed flag storage and precedence behavior.
			var reused target
			if err := resolve(specs, fs, &reused, tc.path, lookupEnv, nil); err != nil {
				t.Fatalf("resolve() reused error = %v", err)
			}
			if reused.value != tc.want {
				t.Errorf("reused value = %q, want %q", reused.value, tc.want)
			}
		})
	}
}

func TestResolveFinalizesFlagValueBeforeValidation(t *testing.T) {
	type target struct {
		value string
	}

	var events []string
	fs := ff.NewFlagSet("test")
	specs := []spec[target]{
		stringField("value", "default", func(c *target) *string { return &c.value }, func(_ string, value string) error {
			events = append(events, "validate:"+value)
			if value != "finalized" {
				return fmt.Errorf("validate saw %q", value)
			}
			return nil
		}, "test value"),
	}
	for _, s := range specs {
		s.register(fs)
	}
	if err := ff.Parse(fs, []string{"--value", "flag"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	var got target
	err := resolve(specs, fs, &got, "", func(string) (string, bool) { return "", false }, func(c *target) error {
		events = append(events, "finalize:"+c.value)
		c.value = "finalized"
		return nil
	})
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	wantEvents := []string{"finalize:flag", "validate:finalized"}
	if !slices.Equal(events, wantEvents) {
		t.Errorf("events = %v, want %v", events, wantEvents)
	}
}

func TestTypedFieldsResolveValues(t *testing.T) {
	type target struct {
		duration time.Duration
		text     string
		bytes    int64
		retries  int
		enabled  bool
		secret   string
	}

	path := filepath.Join(t.TempDir(), "copilotd.toml")
	contents := []byte("duration = \"2s\"\ntext = \"file\"\nbytes = 2\nretries = 2\nenabled = true\nsecret = \"file-secret\"\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	fs := ff.NewFlagSet("test")
	specs := []spec[target]{
		durationField("duration", time.Second, inSeconds, func(c *target) *time.Duration { return &c.duration }, positive, "duration"),
		stringField("text", "default", func(c *target) *string { return &c.text }, nil, "text"),
		int64Field("bytes", 1, func(c *target) *int64 { return &c.bytes }, positive, "bytes"),
		intField("retries", 1, func(c *target) *int { return &c.retries }, nonNegative, "retries"),
		boolField("enabled", false, func(c *target) *bool { return &c.enabled }, "enabled"),
		secretStringField("secret", func(c *target) *string { return &c.secret }, required, "secret"),
	}
	for _, s := range specs {
		s.register(fs)
	}
	args := []string{
		"--duration", "4s",
		"--text", "flag",
		"--bytes", "4",
		"--retries", "4",
		"--enabled=true",
		"--secret", "flag-secret",
	}
	if err := ff.Parse(fs, args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	env := map[string]string{
		"COPILOTD_DURATION": "3s",
		"COPILOTD_TEXT":     "env",
		"COPILOTD_BYTES":    "3",
		"COPILOTD_RETRIES":  "3",
		"COPILOTD_ENABLED":  "false",
		"COPILOTD_SECRET":   "env-secret",
	}

	var got target
	if err := resolve(specs, fs, &got, path, func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}, nil); err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	want := target{
		duration: 4 * time.Second,
		text:     "flag",
		bytes:    4,
		retries:  4,
		enabled:  true,
		secret:   "flag-secret",
	}
	if got != want {
		t.Errorf("resolved target = %+v, want %+v", got, want)
	}
}

func TestSecretFieldOmitsLogAttribute(t *testing.T) {
	type target struct {
		value string
	}
	cfg := target{value: "classified"}
	s := secretStringField("secret", func(c *target) *string { return &c.value }, nil, "secret")
	if _, include := s.logAttr(&cfg); include {
		t.Error("logAttr() include = true, want secret omitted")
	}
}

func TestFieldWithoutCheckSkipsValidation(t *testing.T) {
	type target struct {
		value string
	}
	cfg := target{value: "anything"}
	s := stringField("value", "", func(c *target) *string { return &c.value }, nil, "value")
	if err := s.validate(&cfg); err != nil {
		t.Errorf("validate() error = %v, want nil", err)
	}
}

func TestDurationUnitFormatsExactMultiplesAndFallsBack(t *testing.T) {
	tests := []struct {
		name string
		unit durationUnit
		in   time.Duration
		want string
	}{
		{name: "seconds exact", unit: inSeconds, in: 600 * time.Second, want: "600s"},
		{name: "hours exact", unit: inHours, in: 24 * time.Hour, want: "24h"},
		{name: "zero in seconds", unit: inSeconds, in: 0, want: "0s"},
		{name: "zero in hours", unit: inHours, in: 0, want: "0h"},
		// Only an override can produce a value the declared unit cannot state
		// exactly; Go's own notation is the lossless fallback.
		{name: "sub-unit remainder in seconds", unit: inSeconds, in: 1500 * time.Millisecond, want: "1.5s"},
		{name: "sub-unit remainder in hours", unit: inHours, in: 90 * time.Minute, want: "1h30m0s"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.unit.format(tc.in); got != tc.want {
				t.Errorf("format(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidatorsReturnExactMessages(t *testing.T) {
	tests := []struct {
		name string
		run  func() error
		want string
	}{
		{
			name: "one of",
			run:  func() error { return oneOf([]string{"debug", "info", "warn", "error"})("log-level", "trace") },
			want: `invalid log-level "trace": must be one of debug, info, warn, error`,
		},
		{
			name: "positive",
			run:  func() error { return positive("shutdown-timeout", time.Duration(0)) },
			want: "invalid shutdown-timeout 0s: must be positive",
		},
		{
			name: "non-negative",
			run:  func() error { return nonNegative("startup-mint-retries", -1) },
			want: "invalid startup-mint-retries -1: must be >= 0",
		},
		{
			name: "required",
			run:  func() error { return required("apikey", " \t") },
			want: "apikey is required: set --apikey, COPILOTD_APIKEY, or apikey in the config file",
		},
		{
			name: "valid address",
			run:  func() error { return validAddr("addr", "localhost:70000") },
			want: `invalid addr "localhost:70000": port must be an integer in [0,65535]`,
		},
		{
			name: "bare version",
			run:  func() error { return bareVersion("vscode-version", "latest") },
			want: `invalid vscode-version "latest": must be major.minor.patch with optional prerelease or build suffixes`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("validator error = nil, want error")
			}
			if err.Error() != tc.want {
				t.Errorf("validator error = %q, want %q", err, tc.want)
			}
		})
	}
}
