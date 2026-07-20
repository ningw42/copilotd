# Triage Labels

The engineering skills use five canonical triage roles. This table maps each
role to the label configured in GitHub.

| Label in mattpocock/skills | Label in our tracker | Meaning |
| --- | --- | --- |
| `needs-triage` | `needs-triage` | Maintainer needs to evaluate this issue |
| `needs-info` | `needs-info` | Waiting on the reporter for information |
| `ready-for-agent` | `ready-for-agent` | Fully specified and ready for an AFK agent |
| `ready-for-human` | `ready-for-human` | Requires human implementation |
| `wontfix` | `wontfix` | Will not be actioned |

When a skill names a canonical role, use the corresponding GitHub label from
this table.

## Type labels

Beyond the triage roles above, the tracker uses structural **type labels** that
describe an issue's shape rather than its triage state.

| Label | Meaning |
| --- | --- |
| `epic` | Parent tracking issue decomposed into GitHub sub-issues |

A type label is orthogonal to a triage role: an `epic` still carries its own
triage label (for example `ready-for-agent`). See `issue-tracker.md` for how an
`epic`-labelled parent anchors a wayfinding map.
