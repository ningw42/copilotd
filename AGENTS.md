# Agent instructions

## Development environment

The Go toolchain is provided by the Nix development shell rather than the host
environment. Run Go commands through `nix develop`, for example:

- `nix develop -c go test ./...`
- `nix develop -c go test -race ./... -count=1`

Use `nix fmt` to format the tree and `nix flake check` for the complete local
verification suite.

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues; external pull requests are not a triage
request surface. See `docs/agents/issue-tracker.md`.

### Triage labels

Use the canonical `needs-triage`, `needs-info`, `ready-for-agent`,
`ready-for-human`, and `wontfix` labels. See `docs/agents/triage-labels.md`.

### Domain docs

This is a single-context repository with `CONTEXT.md` and `docs/adr/` at the
root. See `docs/agents/domain.md`.
