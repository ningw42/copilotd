package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ningw42/copilotd/internal/impersonation"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/fftoml"
)

// spec is the type-erased face of one setting. The resolve engine operates on
// heterogeneous settings only through this interface.
type spec[C any] interface {
	key() string
	register(fs *ff.FlagSet)
	applyDefault(*C)
	applyOverlay(target *C, raw, source string) error
	applyFlag(target *C, set map[string]bool)
	logAttr(target *C) (slog.Attr, bool)
	validate(target *C) error
}

// field is the single typed implementation of a config setting descriptor.
// Tables are registered once, then reused so stored continues to point at the
// value populated by ff.Parse.
type field[C, T any] struct {
	name, usage string
	def         T
	get         func(*C) *T
	parse       func(string) (T, error)
	reg         func(*ff.FlagSet, string, T, string) *T
	logf        func(string, T) slog.Attr
	check       func(key string, v T) error
	secret      bool
	stored      *T
}

// configPathField is the bootstrap-only --config carve-out. It participates in
// registration so it retains its help position, but resolution is handled by
// resolveConfigPath before the descriptor engine loads the selected file.
type configPathField[C any] struct {
	stored *string
}

func (f *configPathField[C]) key() string { return "config" }

func (f *configPathField[C]) register(fs *ff.FlagSet) {
	f.stored = fs.StringLong("config", "", "path to an optional TOML config file")
}

func (*configPathField[C]) applyDefault(*C) {}

func (*configPathField[C]) applyOverlay(*C, string, string) error { return nil }

func (*configPathField[C]) applyFlag(*C, map[string]bool) {}

func (*configPathField[C]) logAttr(*C) (slog.Attr, bool) { return slog.Attr{}, false }

func (*configPathField[C]) validate(*C) error { return nil }

func (f *configPathField[C]) flagValue() string { return *f.stored }

func (f *field[C, T]) key() string { return f.name }

func (f *field[C, T]) register(fs *ff.FlagSet) {
	f.stored = f.reg(fs, f.name, f.def, f.usage)
}

func (f *field[C, T]) applyDefault(target *C) {
	*f.get(target) = f.def
}

func (f *field[C, T]) applyOverlay(target *C, raw, source string) error {
	value, err := f.parse(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q from %s: %w", f.name, raw, source, err)
	}
	*f.get(target) = value
	return nil
}

func (f *field[C, T]) applyFlag(target *C, set map[string]bool) {
	if set[f.name] {
		*f.get(target) = *f.stored
	}
}

func (f *field[C, T]) logAttr(target *C) (slog.Attr, bool) {
	if f.secret {
		return slog.Attr{}, false
	}
	return f.logf(f.name, *f.get(target)), true
}

func (f *field[C, T]) validate(target *C) error {
	if f.check == nil {
		return nil
	}
	return f.check(f.name, *f.get(target))
}

func stringField[C any](name, def string, get func(*C) *string, check func(string, string) error, usage string) spec[C] {
	return newStringField(name, def, get, check, usage)
}

func newStringField[C any](name, def string, get func(*C) *string, check func(string, string) error, usage string) *field[C, string] {
	return &field[C, string]{
		name:  name,
		usage: usage,
		def:   def,
		get:   get,
		parse: func(raw string) (string, error) { return raw, nil },
		reg:   func(fs *ff.FlagSet, name, def, usage string) *string { return fs.StringLong(name, def, usage) },
		logf:  slog.String,
		check: check,
	}
}

// durationUnit fixes the notation a duration setting presents its default in,
// so --help and CONFIGURATION.md state one value one way. Go's own
// Duration.String() normalizes to the largest units ("600s" prints as "10m0s"),
// which is what drove the two docs apart; declaring the unit per row keeps them
// together. Presentation only — every duration still parses any Go duration
// form.
type durationUnit struct {
	suffix string
	size   time.Duration
}

var (
	inSeconds = durationUnit{suffix: "s", size: time.Second}
	inHours   = durationUnit{suffix: "h", size: time.Hour}
)

// format renders d in the declared unit, falling back to Go's notation when the
// unit cannot express d exactly — a value only an override can produce.
func (u durationUnit) format(d time.Duration) string {
	if u.size <= 0 || d%u.size != 0 {
		return d.String()
	}
	return strconv.FormatInt(int64(d/u.size), 10) + u.suffix
}

// durationFlagValue is the flag.Value behind every duration setting. ff renders
// a flag's help default from Value.String() (snapshotted at registration), so
// owning String() is what lets a row choose its notation. Set stays
// time.ParseDuration, leaving accepted input identical to the ff-supplied value
// this replaces.
type durationFlagValue struct {
	ptr  *time.Duration
	unit durationUnit
}

func (v *durationFlagValue) String() string {
	if v == nil || v.ptr == nil {
		return ""
	}
	return v.unit.format(*v.ptr)
}

func (v *durationFlagValue) Set(raw string) error {
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return err
	}
	*v.ptr = parsed
	return nil
}

// registerDuration is the fs.DurationLong equivalent for a unit-aware value.
// The placeholder is explicit because ff would otherwise derive it from the
// value's type name and print something other than DURATION. Panics on error,
// as ff's own typed helpers do.
func registerDuration(fs *ff.FlagSet, name string, def time.Duration, unit durationUnit, usage string) *time.Duration {
	stored := def
	if _, err := fs.AddFlag(ff.FlagConfig{
		LongName:    name,
		Usage:       usage,
		Value:       &durationFlagValue{ptr: &stored, unit: unit},
		Placeholder: "DURATION",
	}); err != nil {
		panic(err)
	}
	return &stored
}

func durationField[C any](name string, def time.Duration, unit durationUnit, get func(*C) *time.Duration, check func(string, time.Duration) error, usage string) spec[C] {
	return &field[C, time.Duration]{
		name:  name,
		usage: usage,
		def:   def,
		get:   get,
		parse: time.ParseDuration,
		reg: func(fs *ff.FlagSet, name string, def time.Duration, usage string) *time.Duration {
			return registerDuration(fs, name, def, unit, usage)
		},
		logf:  slog.Duration,
		check: check,
	}
}

func int64Field[C any](name string, def int64, get func(*C) *int64, check func(string, int64) error, usage string) spec[C] {
	return &field[C, int64]{
		name:  name,
		usage: usage,
		def:   def,
		get:   get,
		parse: func(raw string) (int64, error) { return strconv.ParseInt(raw, 10, 64) },
		reg: func(fs *ff.FlagSet, name string, def int64, usage string) *int64 {
			return fs.Int64Long(name, def, usage)
		},
		logf:  slog.Int64,
		check: check,
	}
}

func intField[C any](name string, def int, get func(*C) *int, check func(string, int) error, usage string) spec[C] {
	return &field[C, int]{
		name:  name,
		usage: usage,
		def:   def,
		get:   get,
		parse: strconv.Atoi,
		reg: func(fs *ff.FlagSet, name string, def int, usage string) *int {
			return fs.IntLong(name, def, usage)
		},
		logf:  slog.Int,
		check: check,
	}
}

func boolField[C any](name string, def bool, get func(*C) *bool, usage string) spec[C] {
	return &field[C, bool]{
		name:  name,
		usage: usage,
		def:   def,
		get:   get,
		parse: strconv.ParseBool,
		reg: func(fs *ff.FlagSet, name string, def bool, usage string) *bool {
			return fs.BoolLongDefault(name, def, usage)
		},
		logf: slog.Bool,
	}
}

func secretStringField[C any](name string, get func(*C) *string, check func(string, string) error, usage string) spec[C] {
	f := newStringField(name, "", get, check, usage)
	f.secret = true
	return f
}

func positive[T ~int64 | ~int](key string, value T) error {
	if value <= 0 {
		return fmt.Errorf("invalid %s %v: must be positive", key, value)
	}
	return nil
}

func nonNegative[T ~int64 | ~int](key string, value T) error {
	if value < 0 {
		return fmt.Errorf("invalid %s %v: must be >= 0", key, value)
	}
	return nil
}

func oneOf(allowed []string) func(key, value string) error {
	return func(key, value string) error {
		if !slices.Contains(allowed, value) {
			return fmt.Errorf("invalid %s %q: must be one of %s", key, value, strings.Join(allowed, ", "))
		}
		return nil
	}
}

func required(key, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required: set --%s, %s, or %s in the config file", key, key, envVarName(key), key)
	}
	return nil
}

func validAddr(key, value string) error {
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", key, value, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return fmt.Errorf("invalid %s %q: port must be an integer in [0,65535]", key, value)
	}
	return nil
}

func bareVersion(key, value string) error {
	if !impersonation.IsBareVersion(value) {
		return fmt.Errorf("invalid %s %q: must be major.minor.patch with optional prerelease or build suffixes", key, value)
	}
	return nil
}

// resolve owns the complete config ordering in one place: defaults, TOML,
// environment, flags, then validation.
func resolve[C any](specs []spec[C], fs *ff.FlagSet, target *C, path string, lookupEnv func(string) (string, bool)) error {
	for _, s := range specs {
		s.applyDefault(target)
	}

	if path != "" {
		values, err := parseTOMLKeys(path)
		if err != nil {
			return err
		}
		for _, s := range specs {
			if raw, ok := values[s.key()]; ok {
				if err := s.applyOverlay(target, raw, "config file"); err != nil {
					return err
				}
			}
		}
	}

	for _, s := range specs {
		if raw, ok := lookupEnv(envVarName(s.key())); ok {
			if err := s.applyOverlay(target, raw, "env"); err != nil {
				return err
			}
		}
	}
	set := setFlags(fs)
	for _, s := range specs {
		s.applyFlag(target, set)
	}

	for _, s := range specs {
		if err := s.validate(target); err != nil {
			return err
		}
	}
	return nil
}

func parseTOMLKeys(path string) (map[string]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config file: %w", err)
	}
	defer fh.Close()

	values := make(map[string]string)
	if err := fftoml.Parse(fh, func(name, value string) error {
		values[name] = value
		return nil
	}); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}
	return values, nil
}
