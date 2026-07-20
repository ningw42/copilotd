# Issue tracker: GitHub

Issues and PRDs for this repository live in GitHub Issues at
`ningw42/copilotd`. Use the `gh` CLI for all operations; commands run inside the
checkout infer the repository from `origin`.

## Conventions

- Create an issue with `gh issue create`.
- Read an issue and its discussion with `gh issue view <number> --comments`.
- List work with `gh issue list`, requesting structured JSON when a skill needs
  to filter labels, state, or comments.
- Comment with `gh issue comment`; change labels with `gh issue edit`.
- Close with `gh issue close`, adding a resolution comment when useful.

## Pull requests as a triage surface

**PRs as a request surface: no.** Issues are the request queue. External pull
requests do not enter the issue-triage state machine automatically.

GitHub shares one number space across issues and pull requests. When a bare
reference is ambiguous, try `gh pr view <number>` and fall back to
`gh issue view <number>`.

## Publishing and dependencies

When a skill says to publish to the issue tracker, create a GitHub issue. Apply
the `ready-for-agent` label to an agent-ready ticket unless instructed
otherwise.

Publish blocker issues before the issues they block. Prefer GitHub's native issue
dependencies, using the blocker's numeric database ID rather than its issue
number. If native dependencies are unavailable, put `Blocked by: #<number>` in
the blocked issue body. A ticket with no blockers says
`None — can start immediately`.

Wayfinding maps use an `epic`-labelled parent issue and GitHub sub-issues where
available; fall back to a task list in the parent body when sub-issues are not
enabled. The `epic` label marks the parent tracking issue — the one holding the
overall spec that is decomposed into sub-issues; it is a structural type label,
orthogonal to the triage roles, so an epic still carries its own triage label
(for example `ready-for-agent`). See `triage-labels.md`.
