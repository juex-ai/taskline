---
name: taskline-management
description: |
  Use whenever the user wants to track agent work as structured tasks
  inside a project — capturing a feature, bug, or docs task, sequencing dependent
  work, picking the next thing to pull, advancing a task through the
  pending → start → spec → dev → test → review → done lifecycle, recording
  progress, or asking "what's left?". Trigger phrases include "create
  a task", "add a feature", "what should I work on next", "block this
  task on …", "mark X as in review / done", "show me the open bugs",
  "park this for later", and any project / kanban / backlog management
  ask. Use even when the user doesn't say "task" or "taskline" —
  phrases like "let's plan this", "queue this up", "track this",
  "what's runnable now" all qualify. If the user explicitly invokes
  this skill with no other instructions, treat it as "work the current
  project queue" and proactively drain runnable tasks to completion.
  Skip for one-off todo notes with no state, dependencies, or follow-up
  — just answer those directly.
version: 0.18.4
---

# taskline — task management for AI agents

The `taskline` CLI is your only interface to taskline. It tracks
projects and the tasks (features / bugs / docs) inside them, enforces a
seven-state lifecycle (`pending → start → spec → dev → test → review → done`),
models inter-task dependencies as a DAG, and answers "what's runnable
now?".

**Always go through the CLI.** Don't `curl` anywhere, don't try to read
or write the database, don't shell out to internal endpoints — even if
the CLI doesn't expose the exact verb you want. If you find a real
gap, file a taskline task to extend the CLI; don't work around it.
Where taskline runs and how it stores data is not your concern.

The CLI is built for agents, not humans at a terminal:

- JSON on stdout when not a TTY (your case). Pass `--format json` to
  force it; you almost never want `--format table`.
- Stable exit codes (0 success, non-zero error). Diagnostics on stderr.
- One subcommand per verb. No interactive prompts.
- If a command fails with "connection refused" or similar, tell the
  user — don't try to start anything yourself.

## When to use

Reach for taskline whenever the user's ask has *structure* — state,
ordering, dependencies, more than one item, "what's next?". Examples:

- "Track this as a feature in `<project>`"
- "What should I pick up next?"
- "Block `<task A>` on `<task B>`"
- "Mark `<id>` review / done"
- "Show me the open bugs / what's still in dev"
- "Wipe the done tasks from `<project>`"
- "Use taskline-management" / "按照 taskline-management skill 执行"

If the user explicitly names or invokes `taskline-management` and gives
no additional instruction, treat that as "work the current project's
runnable queue": resolve the project from `--project`, `$TASKLINE_PROJECT`,
or the current repository name when it is unambiguous. If the project
cannot be resolved, ask only for the project name. Once a project is
known, export `TASKLINE_PROJECT` for the session or pass `--project`
on every project-scoped command. Before doing queue work, make sure
the current working directory has a valid agent identity by running
`taskline status --format json`. Register only when it reports
`"registered": false`, then run status again. If status fails because
the local identity or token is invalid, stop and fix that identity
instead of registering another name over it. Then keep pulling
`taskline task next --claim --format json` after each completed task
until it returns the literal `null`. A task is not yours until that
claim command succeeds and the returned `owner` equals the agent name
registered in this directory. Do not stop after one task or one PR
unless the runnable queue is exhausted or a real blocker prevents
progress.

Skip taskline when the user just wants a one-line note, a scratch
todo, or an answer that doesn't survive past this turn — reply
directly. taskline is the wrong tool for content that has no
follow-up.

## Environment

```bash
export TASKLINE_PROJECT="demo"   # default project so you can omit --project
taskline status --format json
taskline register --name "agent-a"  # only when registered=false
taskline status --format json
```

`--project` overrides `$TASKLINE_PROJECT`. A project is referenced by
**name** (`demo`) or **id** (`9b…uuid`) — both work everywhere.
Export `TASKLINE_PROJECT` once at the start of a session that's
focused on a single project.

`taskline register --name <agent>` writes `.config/taskline/agent.json`
in the current working directory. That file contains the bearer token
used by claim, heartbeat, release, and normal update flows; it is
intentionally not global because multiple agents may share one machine.
If a claiming command fails with `agent identity required`, register
the current directory first, then verify it with `taskline status`.
Do not pass or invent owner strings; the
server derives owner from the registered token.

## Domain model

Canonical vocabulary, lifecycle semantics, claims, leases, dependencies, queue
ordering, labels, attachments, and event invariants live in the repository's
[`DOMAIN.md`](../../DOMAIN.md). Treat that document as the stable domain
contract and keep this skill focused on CLI sequencing and the stricter agent
delivery policy.

Every mutation also appends a task history event with `actor`, `action`,
`summary`, structured `details`, and `created_at`. A registered agent token is
recorded as that agent name; otherwise the actor is `web`, `cli`, or `api`.
Use `taskline task history <id>` whenever you need durable operation context or
the exact before/after values for title, description, state, type, priority, or
labels.

Before updating state, claiming work, or interpreting a queue result, use the
definitions in `DOMAIN.md`. In particular, a preview is never permission to
start, lease expiry is not an automatic release, and server evidence gates are
not a substitute for this skill's review and CI requirements.

## CLI cheat sheet

`-h` on any subcommand prints flags. This is the full agent surface.

### Agent preflight

```bash
taskline status --format json
taskline register --name agent-a  # only when status says registered=false
taskline status --format json
```

Status reports CLI version, server health, checkout-local config directory,
default project, registered agent, and the agent's current live claims across
projects. A configured identity must be accepted by the server; an invalid or
stale token is an error, not an unregistered state. Registration with an
already-valid token is rejected so one agent cannot accidentally replace
another checkout identity.

### Projects

```bash
taskline project create --name demo --description "first project"
taskline project list
```

### Tasks

```bash
# Create (defaults to 'start' state — immediately runnable)
taskline task create --project demo --title "first task" --type feature --priority 1
taskline task create --project demo --title "labeled task" --label backend --label ui

# Create and park in 'pending' (won't show up in `task next`)
taskline task create --project demo --title "later idea" --auto-start=false

# List (filter by state with comma-separated names)
taskline task list --project demo
taskline task list --project demo --state start,dev,test
taskline task list --project demo --mine
taskline task list --project demo --unclaimed
taskline task list --project demo --runnable --label backend
taskline task list --project demo --runnable --mine

# Pick / inspect
taskline task next --project demo            # preview only; does not reserve work
taskline task next --project demo --claim --lease 6h
taskline task next --project demo --claim --label backend
taskline task search --project demo fc7a0732 # short id / full id / text matches
taskline task search --project demo "historical context" --limit 10
taskline task get <id>
taskline task history <id>                  # actor, operation, time, before/after

# Mutate (PATCH semantics — only pass the flags you want changed)
taskline task update <id> --state test
taskline task update <id> --priority 5 --description "new prose"
taskline task update <id> --label review --label frontend   # replace labels
taskline task update <id> --add-label review --remove-label triage
taskline task update <id> --append-description "new note"
taskline task update <id> --clear-labels                    # remove labels
taskline task update <id> --state done --if-state review
taskline task update <id> --state pending --force            # manual correction
taskline task delete <id>                    # cascades dep/attachment metadata; files remain

# Multi-agent ownership
taskline task claim <id> --lease 2h
taskline task heartbeat <id> --lease 6h
taskline task release <id>
taskline task release <id> --force           # manual recovery

# Dependencies
taskline task depend <id> --on <other-id>
taskline task undepend <id> --on <other-id>

# Image attachment (any binary)
taskline task upload <id> --file ./screenshot.png

# Markdown docs (stage deliverables, notes, reports)
taskline task doc create <task-id> --title "Spec" --file ./spec.md
taskline task doc get <doc-id>
taskline task doc update <doc-id> --title "Test Report" --file ./test-report.md
taskline task doc delete <doc-id>

# Link (PR, external design doc, ticket, merged commit — any URL to remember)
taskline task link <task-id> --url https://example.com/pr/42 --label "PR #42"

# Remove a link by its id (links are returned inline on `task get`)
taskline task unlink <link-id>
```

Delete returns `{"deleted": true, "id": ...}`; depend returns
`{"task_id": ..., "depends_on": [...]}`. Pipe to `jq` freely.

### Multi-agent claim flow

Run `taskline status --format json` first and confirm the registered agent
identity. Register only when status explicitly reports `registered=false`.
Use `task next --claim` when more than one agent may pull from the same
project. Plain `task next` is a read-only preview and does **not**
reserve work. Never begin implementation from a plain `task next`
result; claim first.

`task next --claim` atomically selects the first claimable task in the canonical
queue order, sets claim metadata, and returns it. Same-owner claims are
preferred so a restarted agent can pick up its own unfinished work first.

The default lease is 6h. Use a shorter `--lease` for short tasks. Normal
`task update` commands by the current owner renew the lease;
`task heartbeat <id>` renews without changing task content. `task release <id>`
gives work back immediately. Expired leases become reclaimable without a
background worker, but stored owner metadata remains until release or a later
claim. A different agent must claim the task before attempting normal updates.

Do not infer your identity from a returned task's `owner` field. That
field says who currently owns the task; your identity is the agent
registered in `.config/taskline/agent.json` under the current working
directory. If a task is claimed by a different live owner, pull a
different task with `task next --claim` instead of trying to act as
that owner.

Use repeated `--label` flags when agents should consume different labeled
subsets inside one project. Example:
`task next --claim --label backend` atomically claims only runnable tasks
tagged `backend`; adding more labels narrows the filter with AND semantics.
The same label filter is available on `task list --runnable` for previews.

### Task docs and links

As you walk a task through the playbook you'll generate artifacts that
belong with it. Use task docs for Markdown content owned by the task
itself, and links for external URLs such as PRs, commits, design tools,
or chat threads. Do not keep stage deliverables only in chat history.

Keep a local working copy of each `Spec`, `Dev Notes`, `Test Report`, and
`Review Report`. Edit that copy incrementally, then upload it with
`taskline task doc update <doc-id> --file <path>` once per logical batch. Do
not rewrite and upload the full document separately for every review comment.

Task docs are first-class Markdown files. They surface inline on
`task get` with `url` fields under `/api/v1/docs/<doc-id>/content`;
fetch full editable content with `taskline task doc get <doc-id>`.
Create or update the stage doc before advancing out of the matching
stage:

- **spec → dev:** `Spec` doc with product design, scope, UX or interaction
  behavior, acceptance criteria, and verification scenarios. Detailed
  technical design and implementation planning begin in `dev`.
- **dev → test:** `Dev Notes` doc summarizing implementation, issues
  encountered, and any divergence from the spec/technical design.
- **test → review:** `Test Report` doc reviewing test cases, module
  tests, real e2e/API/CLI/browser/device checks, agent evaluation,
  pass rate, failures, and whether failures require returning to dev.
- **review → done:** `Review Report` doc covering PR comments, CI
  status, whether the implementation still matches the original design,
  and any justified design updates.

Recommended moments to call it:

- **spec/dev/test/review**: create or update the matching Markdown doc.
- **test → review**: link the PR URL just after `gh pr create` ("PR #N").
- **review → done**: update the Review Report and any merged-commit or
  post-mortem links before changing state. The attached PR link itself is the
  authoritative merge/review/CI evidence.

Docs and links surface inline on `task get` and in the web detail view.
There is no limit on how many docs or links a task can hold; favour
adding too many over too few — they're cheap to remove later.

## Stage playbook — "work the queue"

When the user says "work the queue" / "do the next task" / "keep
going through the backlog", or explicitly invokes this skill without
more instructions:

1. Run `taskline task next --project <p> --claim --format json`.
2. The CLI emits the bare task object (`id`, `title`, `state`, … as
   top-level fields) on successful claim, or the literal `null` when
   nothing is currently claimable. If you see `null`, report there's
   nothing runnable/claimable and stop. If the returned task has an
   `owner` different from the agent registered in this working
   directory, stop; that is not your task.
3. Read `title`, `description`, any `docs`, and any `images`. Each doc
   includes a raw Markdown `url` under `/api/v1/docs/<doc-id>/content`;
   each image includes a `url` under `/api/v1/images/<image-id>`. Fetch
   and surface them when they are material to the task. When a task
   references a short id, previous work, or historical context, use
   `taskline task search --project <p> "<query>" --format json` to find
   the related task before relying on memory or chat history.
4. Walk the task through the stages below in order. Each stage has the
   same shape: **Trigger** (what just happened) → **Actions** (do
   these now) → **Advance** (literal CLI command to move state) →
   **Skip when** (escape clause).
5. Loop back to step 1 — don't pause to ask the user whether to
   continue.

Higher-order capabilities (brainstorming, planning, code review) below are
optional thinking methods, not mandatory external skill invocations. During
autonomous queue work, do not invoke a skill that requires user approval or
writes process files into the repository. Use a full interactive skill only
when the user explicitly requests that workflow.

### start → spec

- **Trigger:** you just successfully claimed the task from the queue.
- **Actions:**
  1. `git checkout main && git pull`
  2. `git checkout -b feature/<short-kebab-slug>` (slug from the title;
     keep it under ~30 chars).
  3. Confirm `git status` is clean.
- **Advance:** `taskline task update <id> --state spec`
- **Skip when:** the change qualifies as fast-path (see below) — go
  straight to dev.

### spec → dev

- **Trigger:** branch exists, title + description loaded.
- **Actions:**
  1. Clarify the product contract from the task description, project
     docs, and code context: user need, scope, non-goals, UX or
     interaction behavior, and acceptance criteria. Do not ask the user
     for routine design approval; ask only when missing information
     makes safe implementation impossible.
  2. Capture that contract in a `Spec` task doc before advancing. The
     doc must include product design, scope, UX or interaction behavior,
     acceptance criteria, and verification scenarios. Detailed technical
     design, IDL/API decisions, and the implementation plan belong to `dev`.
     If an existing plan already captures the product contract, upload the
     relevant content rather than duplicating it in the task description.
- **Advance:** `taskline task update <id> --state dev`
- **Skip when:** the change is mechanical (rename, formatting,
  one-line config) — go straight to dev.

### Architecture review when warranted

For routine product or technical choices, do not pause for user approval.
After identifying viable approaches, choose the simplest one that fits the
product goal and perform a second-pass boundary and testability check yourself.

Use an architecture subagent only for cross-module work with genuine
implementation alternatives where an independent boundary review could change
the approach. Give it the task title, description, options, recommendation, and
relevant repository constraints. Ask it to check for over-engineering, unclear
ownership, hidden coupling, performance risks, testability gaps, and violations
of the project's philosophy. The final choice should be simple, declarative,
readable, performant enough for the expected workload, and aligned with existing
module boundaries.

Ask the user only when the product intent is genuinely unknowable from
the task, the decision has external/business consequences, credentials
or destructive permissions are missing, or the safe implementation
cannot proceed without information that is not in the repo or taskline
task. In all other cases, record the chosen approach and reason in the
task description or implementation notes, then continue.

### dev → test

- **Trigger:** product spec / acceptance criteria in hand.
- **Actions** (test-first):
  1. Brainstorm the technical approach — list 2-3 implementation options,
     pick one, and name the tradeoff. No human checkpoint.
  2. Apply the architecture-review threshold above. Most tasks need only the
     local second-pass boundary check; use an architecture subagent only when
     the task meets the cross-module / genuine-alternatives condition.
  3. Plan the technical work — architecture boundary, ordered steps, and
     test strategy. Keep routine planning in working context; for multi-step
     handoff work, attach the plan as a Taskline task doc. Do not commit
     repository process-plan files.
  4. Write or extend failing tests for the new behavior.
  5. Implement until the focused tests pass and the behavior is ready
     for full local verification.
  6. Create or update a `Dev Notes` task doc summarizing the
     implementation, issues encountered, and any divergence from the
     `Spec` doc with the reason.
- **Advance:** `taskline task update <id> --state test`
- **Skip when:** never. Implementation must be ready for local
  verification before review begins.

### test → review

- **Trigger:** implementation behavior is complete in the local
  worktree.
- **Actions:**
  1. Review the tests you wrote or touched. Add coverage now if the
     behavior, migration path, CLI surface, or UI state can regress.
  2. Run the full project test suite for whatever you touched.
     For this repo, `make check` is the complete gate. Use
     `make lint MODULE=<server|cli|web>`,
     `make test MODULE=<server|cli|web>`, and
     `make build MODULE=<server|cli|web>` for focused reruns.
     Run `make test-skill` when skill docs changed. Lint / format as
     the project requires.
  3. For taskline itself, or any project with an embedded frontend,
     migrations, or runtime startup behavior, verify against the rebuilt
     running binary rather than only isolated tests.
  4. Self code-review for bugs, dead code, boundary issues.
     (capability: code review — `code-review:code-review`)
  5. Fix anything the review or tests surface; re-run the relevant
     tests after each fix.
  6. Create or update a `Test Report` task doc with reviewed test
     cases, commands/checks run, pass rate, failures, and whether any
     failures require dropping back to `dev`.
  7. Stage and commit. Conventional, minimal messages.
  8. `git push -u origin <branch>`.
  9. `gh pr create` with title, summary, and a test plan.
  10. Attach the PR URL to the task:
     `taskline task link <task-id> --url <pr-url> --label "PR #N"`
     so anyone reading the task later can jump straight to the
     review.
- **Advance:** `taskline task update <id> --state review`
- **Skip when:** never. Tests and a real pushed PR are the gate. The server
  rejects the transition until the PR link has been attached.

### review → done

- **Trigger:** a PR exists for the committed implementation.
- **Actions:**
  1. **Wait only for the latest PR head's CI checks.** Record the current PR
     `headRefOid`, then wait for its configured CI checks to complete. A new
     push invalidates all earlier-head CI and review evidence: record the new
     `headRefOid` and wait for the new head's CI to complete. Do not add a
     separate time-based gate or wait for a review to appear.

     Use the repository's required checks rather than inventing an extra local
     review gate:

     ```bash
     gh pr view <n> --json headRefOid,statusCheckRollup
     gh pr checks <n> --required --watch
     ```

     Rely on repository-configured automatic review. Do not run a local
     review-agent command or manually summon a review bot as part of the
     default flow.
  2. **Refresh comments after CI completes.** Confirm `headRefOid` is still the
     recorded head, then refresh required checks, review summaries, review
     threads, and top-level PR comments. The REST calls below cover summaries
     and comments; use GitHub GraphQL to read each review thread's resolution
     state because inline comments alone do not expose it:

     ```bash
     gh pr view <n> --json headRefOid,reviews,reviewDecision,statusCheckRollup
     gh pr checks <n> --required
     gh api --paginate repos/<owner>/<repo>/pulls/<n>/reviews
     gh api --paginate repos/<owner>/<repo>/pulls/<n>/comments
     gh api --paginate repos/<owner>/<repo>/issues/<n>/comments
     gh api graphql --paginate -F owner=<owner> -F repo=<repo> -F number=<n> -F endCursor=null -f query='query($owner:String!,$repo:String!,$number:Int!,$endCursor:String){repository(owner:$owner,name:$repo){pullRequest(number:$number){reviewThreads(first:100,after:$endCursor){nodes{id isResolved} pageInfo{hasNextPage endCursor}} commits(last:1){nodes{commit{statusCheckRollup{state}}}}}}}'
     ```

     Required checks are the early wait set, not the final merge gate. Inspect
     the same GraphQL aggregate used by the server. The
     `statusCheckRollup { state }` aggregate must be `SUCCESS`, or the rollup
     must be absent, before merge. This accepts GitHub-successful rollups that
     contain neutral or skipped individual checks while still preventing a
     non-required pending or failed rollup from stranding an already-merged PR.

     If there are no reviews or comments, continue to the aggregate rollup and
     merge checks without any further review wait. If reviews or comments do
     exist, apply this finding policy:

     - Treat the initial PR review and the reviews after the first two fix
       commits as the three full-attention rounds. Handle every actionable
       P0/P1/P2/P3 finding in those rounds with a fix or reasoned rebuttal and
       resolve its thread.
     - Starting with the review after fix commit 3, handle only P0/P1. Do not
       change code, provide a substantive reply, or resolve a thread for P2/P3.
     - If a finding has no priority, classify it as P0, P1, P2, or P3 before
       applying the round rule. Mark the GitHub thread with a short first-line
       reply such as `Priority: P2` and record the decision in the local
       `Review Report`. Status notifications and comments with no actionable
       finding do not need classification.
     - P0/P1 always require a fix or reasoned rebuttal, followed by resolving
       the review thread.

     Ordinary review fixes should stay in `review`: edit the local stage docs,
     batch the round into one fix commit, run the relevant tests, push, wait for
     the new head's CI, and refresh the comment surfaces again. Only a material
     change to product scope, architecture, or the chosen solution should
     return the task to `dev` for renewed design and implementation work.
  3. Merge only after the latest head's required CI succeeds, its aggregate
     check rollup satisfies the server-compatible rule above, the current
     comment surfaces have been inspected, and every finding that blocks under
     the current review-fix policy is handled. Pin the inspected head so a
     concurrent push aborts instead of merging unchecked work:

     ```bash
     gh pr merge <n> --squash --delete-branch --match-head-commit <recorded-head-oid>
     ```

     Substitute the project's required merge style when it is not squash, but
     keep `--match-head-commit`.
  4. Confirm the remote result with
     `gh pr view <n> --json state,mergedAt,statusCheckRollup`.
  5. Create or update the local `Review Report`, then upload it once for this
     logical batch. Cover PR comments,
     CI status, merge result, and whether the implementation still matches
     the original design. If not, either update the design doc with the
     justified change or drop back to `dev` for rework.
- **Advance:** `taskline task update <id> --state done` *only after*
  (a) the latest head's required CI is green or N/A, (b) the aggregate
  check-rollup state is `SUCCESS` or absent, (c) the current comment surfaces
  have been inspected, (d) every finding that blocks under the current round
  policy is fixed or rebutted and every P0/P1 review thread is resolved, and
  (e) the inspected head is confirmed merged. A posted review is not required.
  The server queries GitHub and rejects `done` when merge, unresolved P0/P1, or
  CI evidence is incomplete.
- **Drop back to dev** with `taskline task update <id> --state dev` only when
  review changes product scope, architecture, or the chosen solution
  materially. Keep ordinary defect fixes in `review`; do not delete and
  recreate the task.

### done — wrap-up

- **Trigger:** task is `done` after the PR was merged and verified.
- **Actions:**
  1. `git checkout main && git pull`
  2. Delete the local feature branch (gh's `--delete-branch` may have
     done this already).
- The taskline task is already `done`; this stage is repo hygiene.

## Fast path

A task qualifies as fast-path when **all** of:

- single file changed,
- no behavior visible to other code,
- no test scaffolding or new dependency.

Examples: typo in a comment, raising a log level, bumping a constant.
The product/spec work may collapse, but the delivery gates do not:

```
	start → dev → test → review → done
```

Skip a separate Spec when appropriate and keep stage docs concise, but still
use a branch, commit, real push, PR, CI/review, and merge. Documentation never
substitutes for delivery evidence.

## Gotchas

- **`taskline status` fails for an existing identity** — do not register a new
  name over it. Check `TASKLINE_SERVER` and
  `.config/taskline/agent.json`; repair or intentionally remove the stale local
  identity before registering again.
- **`already registered as ...`** — this checkout already has a valid token.
  Run `taskline status` and continue as that agent; do not rotate its identity.
- **Forgot `--project`?** Export `TASKLINE_PROJECT` once at session
  start. Only `task create`, `task list`, `task search`, and
  `task next` accept `--project` — the rest (`get`, `update`, `delete`, `depend`,
  `upload`) operate on the task id directly and reject the flag with
  "unknown flag".
- **`invalid next state "..."`** — you used a name that isn't in
  `pending/start/spec/dev/test/review/done`. The state `created` was
  renamed to `start`, and `design` was renamed to `spec`; don't
  reintroduce old names.
- **`cannot enter review`** — create and push the branch, open a real GitHub
  PR, attach it with the exact `taskline task link ...` command shown in the
  error, then retry the state update.
- **`cannot enter done`** — follow the listed blocker: resolve every P0/P1
  review thread, finish required CI for the latest PR head, confirm its aggregate
  check-rollup state is `SUCCESS` or absent, inspect the current comment
  surfaces, merge the PR, update task docs/links, then retry. Do not use
  `--force`; it cannot bypass delivery evidence.
- **No review or comments after CI** — once the latest head's CI is complete,
  refresh every evidence surface. If nothing requires action, proceed to the
  aggregate rollup and merge checks immediately.
- **A push lands during review** — all CI and review evidence for the previous
  head is stale. Wait for the new head's CI, then refresh the review and comment
  surfaces again.
- **An unprioritized finding appears** — classify it as P0/P1/P2/P3, add a
  first-line `Priority: Pn` reply to the GitHub thread, and record the decision
  in the Review Report before applying the current review-round policy.
- **P2/P3 appears on or after fix commit 3** — do not change code, provide a
  substantive reply, or resolve the thread. The server `done` gate ignores
  unresolved P2/P3. Before fix commit 3, P2/P3 belongs to the three
  full-attention review rounds and must be handled.
- **`state entry verification unavailable`** — GitHub could not be queried.
  Configure `TASKLINE_GITHUB_TOKEN`/`GITHUB_TOKEN`/`GH_TOKEN` for the server or
  run `gh auth login` on the server host, then retry.
- **`dependency would create a cycle`** — the edge would loop back.
  Restructure the graph or pick a different anchor.
- **`project name "X" already exists`** — name collision. Reuse the
  existing project (likely what you wanted) or pick a new name.
- **`error: project required`** — neither `--project` nor
  `$TASKLINE_PROJECT` is set.
- **`task next --claim` returned `null`** — nothing is claimable for
  this registered agent. Either the project is empty, every non-done task is
  blocked, every available task is claimed by another live owner, or
  everything left is parked in `pending`. Run
  `taskline task list --project <p> --state pending,start,spec,dev,test,review`
  to see what's stuck and why. Do not automatically move `pending`
  tasks into `start`; promote them only when the task description,
  dependencies, or the user makes clear that they are ready to run.
- **The user said "remind me to X"** — that's a one-off note, not a
  task. Reply directly; don't create a taskline entry.
