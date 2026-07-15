# CLI Flag Scope Design

## Goal

Make the command-line interface match the command tree:

- `copilotd version` is the only version interface.
- Operational configuration is available to `serve` and `login`, but not to
  informational commands such as `version` and `help`.

## CLI Contract

Remove `--version` and the currently accepted `-version` spelling immediately.
There is no compatibility warning or deprecation period. Both spellings become
invalid and exit non-zero; `copilotd version` continues to print build metadata
and exit zero. `version` accepts no operands. Its output remains exactly one
newline-terminated `VERSION (COMMIT, DATE)` line from `build.String()` on
stdout, with empty stderr; no prefix, field labels, or alternate format is
added.

The parser-native `-h` and `--help` flags remain available on root and every
subcommand as conventional aliases for help. Unlike version, help therefore has
both flag and subcommand forms. The explicit `help` subcommand accepts zero or
one operand: no operand renders general help, and one subcommand name renders
help for that command. Subcommand lookup is case-insensitive in both normal
dispatch and the explicit help form. Generated help advertises the explicit
`help` subcommand; parser-native help flags remain supported but intentionally
unlisted.

Bare `copilotd` continues to render the same general help as `copilotd help` and
exit zero. An unknown subcommand remains an error and points the operator to
`copilotd help`.

Informational paths—bare `copilotd`, `help`, `version`, and built-in help
flags—do not load or validate configuration. They ignore all `COPILOTD_*`
environment settings, including a missing or malformed file named by
`COPILOTD_CONFIG`.

The following settings are shared operational flags rather than root-global
flags:

- `--log-level`
- `--log-format`
- `--log-file`
- `--config`
- `--github-oauth-token-file`

Each is accepted by `copilotd serve` and `copilotd login`, after the subcommand
name. Root, `version`, and `help` do not accept them as flags or list them in
their own help; `help serve` and `help login` do list the target command's
operational flags. Their TOML keys, environment variables, defaults, validation,
and precedence remain unchanged. There is no root-level placement for these
flags: the subcommand name must come first. Both operational commands keep the
same logging and config-file controls because each performs network I/O and
handles credentials; informational commands remain configuration-free.

`serve`, `login`, and `version` accept no operands. `help` alone accepts zero or
one operand, as described above. Every command rejects surplus operands instead
of silently ignoring them.

`--github-oauth-token-file` always identifies the **GitHub OAuth token file**.
For `login` it is the atomic write target; for `serve` it is a token source to
read. The shared name intentionally describes the same artifact despite those
different roles.

Examples:

```text
copilotd serve --github-oauth-token-file /path/to/token
copilotd login --github-oauth-token-file /path/to/token
copilotd version
```

`copilotd --version`, `copilotd -version`, and
`copilotd version --github-oauth-token-file /path/to/token` are invalid. So is
the old root-prefix form `copilotd --config /path/to/config.toml serve`.

## Configuration Structure

Root no longer owns operational flags. Configuration exposes a small internal
common-flag registration unit that declares the five shared settings on a given
subcommand flag set and returns the typed handles needed during resolution.
`RegisterServe` and `RegisterLogin` each compose that unit with their own
command-specific flags.

Registering an equivalent common set separately for each command avoids a
mutable flag set inherited by unrelated commands while keeping names, help
text, defaults, and resolution logic defined in one place. In particular, it
keeps parse/reset state command-local; the shared helper defines flags but does
not share flag instances. Help is derived from the flags registered on each
command, with no separately maintained shared-help metadata. Each resolver
reads only its command's parsed flag set. The existing precedence remains:

```text
command flag > environment > TOML file > default
```

The command tree gives `serve`, `login`, `version`, and `help` independent flag
sets. Root contains no compatibility `version` flag. Dispatch no longer
pre-scans arguments for a version flag; all input goes through normal command
parsing, and `version` is the only explicit CLI path that writes build metadata
to stdout.

The TOML configuration document remains global even though the `--config` flag
that selects it is available only after an operational subcommand. A single
document may contain settings for both commands; `serve` and `login` each apply
only the subset relevant to that command. This change does not split the file or
alter that projection behavior. Existing loaders also continue to ignore keys
they do not consume, whether those keys belong to the other command or are
genuinely unknown.

## Errors and Help

Removed or misplaced flags use the parser's normal unknown-flag error path and
exit code 1. No deprecation message is emitted. In particular, `-version` is
parsed as bundled short flags and may be reported as unknown `-v`; no pre-scan
or compatibility-specific error preserves the removed spelling.

Extra operands to `serve`, `login`, or `version`, or more than one operand to
`help`, are usage errors and exit with code 1 instead of being silently ignored.
All such failures write a concise error identifying the command and the parser's
offending flag fragment or surplus operand to stderr; they do not dump help.
Unknown subcommands additionally retain the existing hint to run
`copilotd help`.

Root parse errors contain exactly one `copilotd:` prefix. The top-level error
translator suppresses its own prefix when `ff/v4` has already applied the root
command name, eliminating output such as `copilotd: copilotd: ...`.
Subcommand parse errors remain fully qualified, for example
`copilotd: serve: ...`.

Command syntax is validated before configuration resolution or side effects.
When an invocation has both an unexpected operand and invalid configuration,
the operand error wins; no configuration file read, network call, listener
creation, or GitHub OAuth token file write is attempted.

General and version help must not list operational flags. Serve and login help
must each list the five shared operational flags in addition to their own
command-specific flags. Both `copilotd help <subcommand>` and
`copilotd <subcommand> --help` render that subcommand's help.

Within `serve` and `login` help, shared operational flags appear first, in the
order listed in the CLI contract, followed by command-specific flags.

Help uses these exact usage shapes:

```text
copilotd <SUBCOMMAND>
copilotd version
copilotd help [SUBCOMMAND]
copilotd serve [FLAGS]
copilotd login [FLAGS]
```

General help lists subcommands in `version`, `help`, `serve`, `login` order,
putting the informational commands first.

## Testing

Tests will cover:

- `copilotd version` prints exactly one `build.String()` line to stdout, leaves
  stderr empty, and exits zero.
- `serve`, `login`, and `version` reject operands; `help` rejects more than one
  operand.
- Invalid flags and operands produce concise stderr errors without a help dump.
- Root parse errors contain one `copilotd:` prefix; subcommand parse errors keep
  the `copilotd: <subcommand>:` qualification.
- Operand errors take precedence over configuration errors and command side
  effects.
- `--version` and `-version` are rejected with a non-zero exit and do not print
  build metadata.
- `-version` follows parser-native bundled-short-flag handling without a special
  compatibility error or pre-scan.
- Root and subcommand `-h`/`--help` forms continue to render help and exit zero.
- Generated help advertises the `help` subcommand without listing the
  parser-native help flags.
- Bare `copilotd` and `copilotd help` render equivalent general help and exit
  zero; unknown subcommands exit non-zero with help guidance.
- Informational paths still succeed when `COPILOTD_CONFIG` names an unreadable
  or malformed file.
- `help <subcommand>` matches command names case-insensitively, just like normal
  dispatch.
- Operational flags are accepted and resolved by both `serve` and `login`.
- Operational flags are rejected by `version` and absent from root/version
  help.
- Serve/login help includes the shared operational flags.
- Serve/login help orders shared operational flags before command-specific
  flags.
- Usage text and general subcommand ordering match the contract above.
- Existing flag/environment/TOML/default precedence remains intact for both
  operational commands.

The obsolete argument pre-scan and its unit tests are removed. The complete Go
test suite and Nix package build provide final regression verification.

## Documentation

The current Phase 1 CLI design is updated to remove the `--version` alias and to
describe the operational settings as shared by `serve` and `login`, rather than
root-global. Phase 0 design material remains historical.

## Out of Scope

This change does not rename settings, alter environment or TOML support, change
configuration precedence, add strict unknown-key validation, or add new commands
or aliases.
