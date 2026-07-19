# copilotd

Run Anthropic messages API and OpenAI responses API off GitHub Copilot

## Codex catalog and auto-review

Codex only fetches the Codex catalog from a self-hosted provider when that
provider uses command authentication. Enable copilotd's opt-in Codex catalog and
choose a reviewer that appears in both Codex's vendored snapshot and Copilot's
live Responses Catalog:

```toml
# copilotd.toml
codex-catalog-enabled = true
codex-auto-review-model = "gpt-5.4"

# Optional: report Copilot's live prompt/context limits instead of the limits
# from the vendored Codex snapshot.
codex-catalog-override-limits = false
```

The corresponding CLI flags are `--codex-catalog-enabled=true`,
`--codex-auto-review-model <slug>`, and
`--codex-catalog-override-limits=true`. Their environment forms are
`COPILOTD_CODEX_CATALOG_ENABLED`, `COPILOTD_CODEX_AUTO_REVIEW_MODEL`, and
`COPILOTD_CODEX_CATALOG_OVERRIDE_LIMITS`. The catalog is inert by default. It is
served in Codex's `{"models":[...]}` shape only when the request has a
`client_version` query parameter, the catalog is enabled, and either a reviewer
or the limits override is configured. Every request performs one fresh Copilot
`/models` fetch; there is no catalog cache or refresh task.

Configure Codex in `~/.codex/config.toml` as a command-auth provider:

```toml
model_provider = "copilotd"

[model_providers.copilotd]
name = "copilotd"
base_url = "http://127.0.0.1:8080/openai/v1"
wire_api = "responses"
# Do not set env_key or requires_openai_auth; either conflicts with [.auth].

[model_providers.copilotd.auth]
command = "printf"
args = ["sk-your-existing-copilotd-key"]
```

At Codex `rust-v0.144.5`, `responses` is the only accepted `wire_api` value.
Codex requests `GET {base_url}/models?client_version=...`, so the example reaches
copilotd's `/openai/v1/models` route. The auth command must print the same
managed API key configured as copilotd's `apikey` (or `--apikey`) to stdout. It
must not print a GitHub OAuth token or Copilot token. In production, replace the
literal `printf` argument with a command or script that reads the existing key
from your secret store.

### Manually resync the vendored Codex snapshot

The embedded snapshot is currently pinned to Codex `rust-v0.144.5`. To
reproduce that snapshot, or advance it by changing `tag` to a reviewed Codex
release, run from the repository root:

```sh
tag=rust-v0.144.5
commit="$(gh api "repos/openai/codex/commits/$tag" --jq .sha)"
snapshot_dir="$(mktemp -d)"

curl --fail --location --silent --show-error \
  "https://raw.githubusercontent.com/openai/codex/$commit/codex-rs/models-manager/models.json" \
  --output "$snapshot_dir/models.json"
curl --fail --location --silent --show-error \
  "https://raw.githubusercontent.com/openai/codex/$commit/LICENSE" \
  --output "$snapshot_dir/LICENSE"
curl --fail --location --silent --show-error \
  "https://raw.githubusercontent.com/openai/codex/$commit/NOTICE" \
  --output "$snapshot_dir/NOTICE"

install -m 0644 "$snapshot_dir/models.json" internal/catalog/codexdata/models.json
install -m 0644 "$snapshot_dir/LICENSE" internal/catalog/codexdata/LICENSE
install -m 0644 "$snapshot_dir/NOTICE" internal/catalog/codexdata/NOTICE
```

Then update
[`PROVENANCE.md`](internal/catalog/codexdata/PROVENANCE.md) with the tag and
resolved commit, and update the pin named in `internal/catalog/codex_snapshot.go`,
ADR 0005, and this guide. Review the snapshot diff for complete `ModelInfo`
entries and run:

```sh
nix develop -c go test ./internal/catalog ./internal/server
nix develop -c go test ./...
nix develop -c go test -race ./...
git diff --check
```

Do not hand-edit `models.json`: complete upstream entries are re-emitted so
Codex retains its own instructions and model behavior. Snapshot freshness is a
manual release task; copilotd deliberately creates no durable snapshot state.
