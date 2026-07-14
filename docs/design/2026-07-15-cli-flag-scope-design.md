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
and exit zero.

The following settings are shared operational flags rather than root-global
flags:

- `--log-level`
- `--log-format`
- `--log-file`
- `--config`
- `--github-oauth-token-file`

Each is accepted by `copilotd serve` and `copilotd login`, after the subcommand
name. They are not displayed or accepted by the root, `version`, or `help`
commands. Their TOML keys, environment variables, defaults, validation, and
precedence remain unchanged.

Examples:

```text
copilotd serve --github-oauth-token-file /path/to/token
copilotd login --github-oauth-token-file /path/to/token
copilotd version
```

`copilotd --version`, `copilotd -version`, and
`copilotd version --github-oauth-token-file /path/to/token` are invalid.

## Configuration Structure

Root no longer owns operational flags. Configuration exposes a small internal
common-flag registration unit that declares the five shared settings on a given
subcommand flag set and returns the typed handles needed during resolution.
`RegisterServe` and `RegisterLogin` each compose that unit with their own
command-specific flags.

Registering an equivalent common set separately for each command avoids a
mutable flag set inherited by unrelated commands while keeping names, help
text, defaults, and resolution logic defined in one place. Each resolver reads
only its command's parsed flag set. The existing precedence remains:

```text
command flag > environment > TOML file > default
```

The command tree gives `serve`, `login`, `version`, and `help` independent flag
sets. Root contains no compatibility `version` flag. Dispatch no longer
pre-scans arguments for a version flag; all input goes through normal command
parsing, and only the `version` command prints build metadata.

## Errors and Help

Removed or misplaced flags use the parser's normal unknown-flag error path and
exit code 1. No deprecation message is emitted.

General and version help must not list operational flags. Serve and login help
must each list the five shared operational flags in addition to their own
command-specific flags.

## Testing

Tests will cover:

- `copilotd version` prints build metadata and exits zero.
- `--version` and `-version` are rejected with a non-zero exit and do not print
  build metadata.
- Operational flags are accepted and resolved by both `serve` and `login`.
- Operational flags are rejected by `version` and absent from root/version
  help.
- Serve/login help includes the shared operational flags.
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
configuration precedence, or add new commands or aliases.
