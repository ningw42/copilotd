# Automation setup

## Kura update dispatch

After `.github/workflows/release.yml` successfully publishes a release and its
assets for a pushed `v*` tag, `trigger-kura-update` dispatches
[`ningw42/kura`'s `auto-update-packages.yml`](https://github.com/ningw42/kura/actions/workflows/auto-update-packages.yml)
on `master`. The dispatch starts Kura's full package updater asynchronously; the
release workflow does not wait for the updater or its resulting PR to complete.

### Manual setup

1. Ensure GitHub Actions is enabled in both repositories and Kura's updater
   workflow is available on `master`.
2. Create a separate [fine-grained personal access token](https://github.com/settings/personal-access-tokens/new)
   restricted to **Only select repositories → kura**, with **Actions: Read and
   write**. Set an expiration and arrange rotation before expiry.
3. Store it as an Actions repository secret named `KURA_WORKFLOW_PAT` in
   **copilotd**, using the hidden interactive prompt:

   ```sh
   gh secret set KURA_WORKFLOW_PAT --repo ningw42/copilotd
   ```

   Do not put the token in a file, command argument, issue, or chat.

The built-in `GITHUB_TOKEN` cannot dispatch workflows in Kura; there is no
fallback. `KURA_WORKFLOW_PAT` is separate from Kura's own `AUTO_UPDATE_PAT`, which
publishes Kura update PRs. The dispatch token needs no Contents or Pull requests
write permission. Its job grants the source repository's `GITHUB_TOKEN` no
permissions, performs no checkout, and executes no repository code. Protect
release-workflow changes: repository secrets are not restricted to one workflow.

### Verification and recovery

After the next successful release, verify that **Trigger Kura package updates**
succeeds and an **Auto-update packages** run appears in Kura on `master`. Dispatch
success confirms only that GitHub accepted the request, not that the package
update or merge succeeded.

A missing, expired, or underprivileged `KURA_WORKFLOW_PAT` fails the dispatch job
after the release has already been published. Fix the secret and rerun the
failed job to retry without republishing the release.

Kura may update other packages in the same run; a no-change run opens no PR. Its
twice-daily schedule remains a fallback if dispatch is unavailable.
