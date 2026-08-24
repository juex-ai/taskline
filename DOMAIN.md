# Domain

The stable language and invariants of taskline. For product rationale see
`PRODUCT.md`; for code structure see `ARCHITECTURE.md`; for repository workflow
see `AGENTS.md`.

This document is the normative description of domain semantics. Runtime code is
the final fact when implementation and documentation disagree, and a behavior
change must update this document in the same change. The agent workflow in
`skills/taskline-management/SKILL.md` may impose stricter delivery policy than
the server itself enforces.

## Ubiquitous language

| Term | Meaning |
| --- | --- |
| **Project** | A named workspace that owns tasks. Project names are unique within one taskline server. |
| **Task** | A project-owned unit of work with an immutable id, title, type, lifecycle state, integer priority, and optional description, labels, dependencies, and attachments. |
| **Task type** | A coarse classification: `feature`, `bug`, or `docs`. Type does not change lifecycle or queue eligibility. |
| **Agent** | A server-registered worker identity. The CLI stores its id, name, server, and bearer token under `.config/taskline/agent.json` in the current working directory. |
| **Owner** | The one agent name currently recorded on a task. An empty owner means the task has no claim metadata. |
| **Claim** | An atomic reservation that records an owner, claim time, and lease expiry. A live claim prevents a different agent from claiming or normally updating the task. |
| **Lease** | The time-bounded live period of a claim. Expiry makes the task reclaimable; it does not clear claim metadata. |
| **Heartbeat** | Renewal of the current owner's lease without changing task content. |
| **Dependency** | A directed edge `task -> prerequisite`. The task waits for the declared prerequisite to reach `done`. |
| **Runnable task** | A task outside `pending` and `done` whose declared direct dependencies are all `done`. |
| **Claimable task** | A runnable task whose owner is empty, whose lease has expired, or whose owner is the requesting agent. |
| **Doc** | A task-owned Markdown attachment. Its content is stored on disk and its metadata is part of task detail. |
| **Link** | An external URL attached to a task, such as a pull request, design, ticket, or merged commit. |
| **Label** | A task-local string used for grouping and queue filtering. Labels preserve order and first spelling while deduplicating case-insensitively. |
| **Event** | An append-only record of a successful mutation, including actor, action, summary, structured details, and timestamp. |

Agent bearer tokens derive identity and owner for CLI operations. They are a
coordination mechanism, not an account or multi-user authorization boundary:
taskline remains a single-user local service, some endpoints permit anonymous
operations, and force operations can bypass claim ownership where explicitly
supported.

## Task lifecycle

The canonical states are:

```text
pending -> start -> spec -> dev -> test -> review -> done
```

| State | Meaning |
| --- | --- |
| `pending` | Parked work. It is intentionally excluded from the runnable queue. |
| `start` | Work offered to agents and ready for initial analysis when its dependencies allow it. |
| `spec` | Product requirements, scope, UX, and acceptance criteria are being made explicit. |
| `dev` | Technical design and implementation are in progress. |
| `test` | Local verification and test review are in progress before PR review and CI. |
| `review` | A real pull request exists and code review and CI are in progress. |
| `done` | Delivery is complete and the qualifying pull request has been merged. |

The arrow shows the normal delivery order, not a one-way transition graph.
Every known state may move to every other known state, including backward or
direct jumps; unknown state names are rejected. A same-state update is an
ordinary mutation and does not rerun target-state entry rules.

No dependency completion, claim change, or background process advances task
state. Tasks created without `auto_start` begin in `pending`; callers move them
explicitly. Entering `done` sets `completed_at`, leaving `done` clears it, and
ordinary edits or heartbeats preserve it. `updated_at` is the latest mutation
time and is not completion evidence.

### Evidence gates

Target-state rules are separate from transition direction. They run only when a
task actually enters the target state.

| Target | Server-enforced entry rule |
| --- | --- |
| `review` | At least one attached canonical GitHub PR URL resolves to an open or merged pull request. |
| `done` | At least one attached PR is merged, has zero unresolved P0/P1 GitHub review threads, and has a successful check rollup or no check rollup. |

A closed, unmerged PR does not qualify. If several PRs are attached, one
qualifying PR is sufficient. The server does not currently prove that the PR
implements the task, require an approval or any posted review, inspect ordinary
PR or issue comments, require unresolved P2/P3 or unprioritized review threads
to be resolved, or require every attached PR to qualify. An unresolved review
thread blocks `done` only when a comment's first non-empty line marks it as P0
or P1. Verification unavailability is distinct from missing evidence. `--force`
can bypass claim ownership where supported, but never these evidence gates.

The agent delivery policy is intentionally stricter: it requires a real push
and PR, required CI plus a server-compatible aggregate check-rollup state for
the latest head, inspection of every review/comment surface after CI completes,
handling of findings according to the current review-round policy, resolution
of every P0/P1 review thread, and a head-pinned merge before `done`. It does not
require that a review be posted or
impose an additional time-based delay when no comments exist. The
mechanical-change fast path may omit a dedicated Spec stage, but it does not
omit those delivery gates. The exact CI-refresh, finding-priority, fast-path,
and stage-artifact rules belong to
`skills/taskline-management/SKILL.md`, not the service state machine.

## Claims and leases

Agent delivery policy requires claiming a task before starting work. A
read-only queue preview is not permission to implement it.

Claiming is an atomic compare-and-update operation. At most one owner string is
stored, and a different agent cannot take over while the lease is live. Claims,
heartbeats, and normal CLI updates derive owner from the bearer token registered
for the current working directory; callers do not provide an owner string.

The default lease is six hours. A heartbeat extends `lease_expires_at` for the
recorded owner without changing content. A normal authenticated update by the
current owner also renews the lease; updating an unclaimed task does not create
a claim. An explicit release clears owner, claim time, and lease expiry; without
force, only the current owner may release.

Lease expiry is lazy and is not an automatic release. No background worker
clears `owner` or `claimed_at`. Once the expiry time is reached, another agent
may atomically claim the task and replace that metadata. Until that happens,
the previous owner can still reclaim or heartbeat it. Another agent should
claim the expired task before updating it; expiry alone does not make stale
owner metadata disappear from normal update guards.

`taskline status` reports only live, non-`pending`, non-`done` claims for the
registered agent. It validates that the checkout-local token belongs to the
configured server before queue work begins.

## Dependencies and queue selection

Dependencies form one acyclic graph over tasks. Self-dependencies and edges
that would close a cycle are rejected; adding an existing edge is idempotent.
The current service does not require both tasks to belong to the same project,
so an edge may cross project boundaries. Deleting either task removes the edge.

Runnable and claimable are separate predicates:

1. A task is runnable when its state is neither `pending` nor `done` and every
   declared direct dependency is `done`.
2. It is claimable for an agent when it is runnable and unclaimed,
   lease-expired, or already owned by that same agent.

The server's runnable-list and next-task endpoints apply both predicates for
the requesting identity. Anonymous previews hide all live claims. Authenticated
previews include the agent's own live claims, so interrupted work can be
resumed, while hiding claims live-owned by others.

Queue order is same-owner first, then higher integer priority, then earlier
creation time. Repeated label filters narrow the queue with case-insensitive
AND semantics. Completing a dependency changes query eligibility only; it does
not mutate the dependent task's state.

## Task-owned context

Docs hold Markdown content owned by the task. Links point to external evidence.
Images are binary task attachments. These resources are returned with task
detail. Deleting a task cascades dependency and attachment metadata in SQLite,
but the current delete path does not remove backing doc or image files. Event
history deliberately survives task deletion.

Every successful task, claim, dependency, image, doc, or link mutation appends
an event. Events preserve operation context and structured before/after details,
but they are not full database time travel.
