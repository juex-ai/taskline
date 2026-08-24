# Architecture

How taskline is wired together. For domain vocabulary and invariants see
`DOMAIN.md`; for the *why* see `PRODUCT.md`; for build/test/contribution
mechanics see `AGENTS.md`.

## Components

```
   ┌──────────────────┐         HTTP /api/v1/*         ┌──────────────────┐
   │   taskline CLI   │  ────────────────────────────▶ │ taskline-server  │
   │  (cobra, JSON-   │ ◀────────────────────────────  │  (Hertz + SQLite)│
   │   first output)  │            JSON                │                  │
   └──────────────────┘                                │  ┌────────────┐  │
                                                       │  │ embedded   │  │
   ┌──────────────────┐         HTTP /api/v1/*         │  │ React UI   │  │
   │   Browser (UI)   │ ◀────────────────────────────▶ │  │ (go:embed) │  │
   │  React + Vite    │       static + REST            │  └────────────┘  │
   └──────────────────┘                                └──────────────────┘
                                                                │
                                                                ▼
                                                       ┌──────────────────┐
                                                       │  ./data/         │
                                                       │   ├ taskline.db  │
                                                       │   ├ images/<id>/ │
                                                       │   └ docs/<id>/   │
                                                       └──────────────────┘
```

One binary (`taskline-server`) serves both the REST API and the React
SPA. SQLite is one file on disk; image attachments live alongside it as
plain files keyed by task id; task docs are Markdown files stored in the
configured docs directory with only file references kept in SQLite.

## Server layering

`server/` is a single Go module with a strict downward-only import
graph:

```
  cmd/taskline-server/         ← process entrypoint, slog, config wiring
       │
       ▼
  api/handler/                 ← Hertz routes, JSON encode/decode, CORS,
       │                         SPA fallback, status-code mapping
       ▼
  internal/service/            ← name resolution (id-or-name), state-machine
       │                         validation, runnable filter orchestration
       ▼
  internal/store/              ← SQLite. CRUD, dep DAG, cycle check.
       │                         Returns ErrNotFound / ErrConflict sentinels.
       ▼
  api/model/                   ← Project, Task, TaskState, TaskType.
                                 Imported by every layer; imports nothing.
```

`internal/config/` is a sibling of service/store: it's loaded by `cmd/`
once and passed through to the handler (for `ImagesDir` and `DocsDir`).

### Why the split

- The handler layer never touches SQL. It maps HTTP ↔ service calls and
  errors ↔ statuses, nothing else.
- The service layer never touches HTTP. It owns invariants (state
  transitions, project resolution by id-or-name) and calls the store.
- The store layer is the only place that knows about SQLite. It returns
  sentinel errors so the handler can map them to status codes without
  string matching.

## Data model

```sql
projects(id, name UNIQUE, description, created_at, updated_at)
tasks   (id, project_id → projects.id, title, description,
         type TEXT constrained to the canonical task types,
         state TEXT constrained to the canonical lifecycle, priority,
         labels JSON string array,
         owner, claimed_at, lease_expires_at, completed_at,
         created_at, updated_at)
task_deps   (task_id → tasks.id, depends_on_task_id → tasks.id,
             PRIMARY KEY(task_id, depends_on_task_id),
             CHECK(task_id ≠ depends_on_task_id))
task_images (id, task_id → tasks.id, filename, mime_type,
             size_bytes, storage_path, uploaded_at)
task_docs   (id, task_id → tasks.id, title, storage_path,
             created_at, updated_at)
task_links  (id, task_id → tasks.id, url, label, created_at)
task_events (id, task_id, actor, action, summary, details JSON, created_at)
```

Attachment and dependency metadata FKs use `ON DELETE CASCADE`, so
`DELETE /api/v1/tasks/:id` removes their database rows without app-level SQL.
The current task-delete path does not remove backing image or doc files.
`task_events.task_id` intentionally has no FK: append-only history remains
queryable by task ID after the task itself is deleted.

Indexes:
- `idx_tasks_project_state(project_id, state)` — list-by-state filter
- `idx_tasks_priority(project_id, priority DESC)` — runnable ordering
- `idx_task_deps_dep(depends_on_task_id)` — reverse-dep traversal
- `idx_task_images_task(task_id)` — task detail attachment lookup
- `idx_task_docs_task(task_id)` — task detail doc lookup
- `idx_task_links_task(task_id)` — task detail link lookup
- `idx_task_events_task_created(task_id, created_at DESC)` — newest history first

Schema lives twice: once at `server/migrations/0001_init.sql` (for tools
that read the migration history) and once at
`server/internal/store/schema/0001_init.sql` (`go:embed`-ed into the
binary so a fresh database can be created without shipping the migrations
directory). Keep them identical.

## Task operation history

Mutation handlers resolve an actor once per request. A valid bearer token wins
and contributes the registered agent name; otherwise `X-Taskline-Client`
distinguishes `web` and `cli`, with `api` as the neutral fallback. The handler
places that actor in the request context and never constructs event payloads.

The service owns the event vocabulary, summaries, task snapshots, and
structured field differences. It synchronously appends a history event after
each successful task, claim, dependency, image, document, or link mutation.
The store owns only JSON persistence and newest-first retrieval. Task update
events retain full before/after values; document content updates record that
content changed without duplicating full Markdown bodies into SQLite.

## State machine

The authoritative lifecycle, transition semantics, completion timestamp, and
evidence gates live in [`DOMAIN.md`](DOMAIN.md#task-lifecycle).

In code, the canonical state set is `model.stateOrder` and
`CanTransitionTo` performs membership validation. The service calls target
entry rules before `store.UpdateTask`; the store persists the already-validated
state and maintains `completed_at`.
Entry rules are registered by target state in
`server/internal/service/workflow.go`. They run only when the state actually
changes, so ordinary edits and same-state updates do not call external
systems. `PullRequestVerifier` is owned by the service package; the concrete
GraphQL adapter lives in `server/internal/github`. This keeps GitHub transport
and credential lookup outside workflow policy and gives future state rules the
same registry without expanding the handler. Missing evidence maps to 409;
temporary verification/authentication failure maps to 503.

For the `done` rule, the GitHub adapter counts only unresolved review threads
whose comments carry a P0/P1 marker on their first non-empty line. The service
receives that count as a transport-neutral fact; unresolved P2/P3 or
unprioritized threads and ordinary top-level PR/issue comments do not block the
server gate.

## Dependency DAG and the runnable query

The normative DAG, runnable, and claimable rules live in
[`DOMAIN.md`](DOMAIN.md#dependencies-and-queue-selection). `task_deps` is a
many-to-many edge table, and the runnable filter is a single SQL query:

```sql
SELECT … FROM tasks t
 WHERE t.project_id = ?
   AND t.state NOT IN ('done','pending')
   AND NOT EXISTS (
         SELECT 1 FROM task_deps d
           JOIN tasks dt ON dt.id = d.depends_on_task_id
          WHERE d.task_id = t.id AND dt.state <> 'done'
   )
 ORDER BY t.priority DESC, t.created_at ASC;
```

Cycle prevention is application-side: before inserting an edge
`(task → dep)`, the store walks `dep`'s transitive deps and refuses if
it can reach `task`. SQLite has no native graph reachability, and the
DAG is small enough that a DFS per insert is fine.

Adding an existing edge is a no-op (the unique-key violation is caught
and swallowed) so dependency-add is idempotent for agents retrying on
network blips.

Task search is project-scoped lexical matching, not a separate index or
vector store. The handler validates `q` / `limit`, the service ranks
short-id, title, description, label, type, and state matches, and the
store remains a task persistence layer. This keeps the search feature in
the same local-first shape as the rest of the product; a future semantic
search feature would need an explicit persistence/indexing design rather
than being hidden inside the current store.

## Web UI delivery

`server/web/embed.go` exposes the bundle via two paths, in priority:

1. **Embedded** (`//go:embed all:dist`) — the production path. `pnpm
   build` writes into `server/web/dist/`; `go build` rolls it into the
   binary. A `.gitkeep` placeholder lets `go:embed` succeed on a fresh
   checkout where `pnpm build` hasn't run yet. The web `prebuild` step
   preserves that placeholder while replacing generated assets, and
   `FS()` detects the placeholder-only case and falls through.
2. **External `./dev-web/`** — if a directory by that name exists in the
   server working directory, it's served from disk. Useful for iterating
   on the UI without rebuilding the server.

When both miss, the server runs API-only and `serveUI` returns 404.

The handler registers API routes first, then mounts `serveUI` as a
catch-all on `NoRoute`. Unknown paths fall through to `index.html` so
the SPA's client-side router handles deep links.

## Web UI state and ordering

The React app keeps project and view selection in the URL without adding
a full router:

- `?project=<name|id>` selects a project and survives reload/share.
  The app prefers writing the project name, but still resolves saved id
  links.
- `?view=graph` opens the dependency graph. Kanban is the default and
  clears the `view` query param.

Kanban column sorting is component-local UI state. The default "next
execution order" mirrors the agent mental model by putting unblocked
tasks before blocked tasks, then sorting by priority and creation time;
other column sort options are browse conveniences. The canonical
runnable ordering remains the server-side SQL query used by `task next`.

The task search dialog is also a derived UI: it calls
`GET /api/v1/projects/:project/tasks/search`, opens the selected task in
the normal editor, and does not own task state.

## CLI ↔ server protocol

The CLI is a thin REST wrapper. `cli/client/client.go` is a hand-written
HTTP client (no codegen, no shared types) so the CLI module can stay
independent of the server module. Domain shapes are duplicated and kept
in sync by hand — drift here is the single most likely place for bugs,
so a CLI-side e2e test suite exercises the round-trip.

Agent identity and claim semantics are defined in
[`DOMAIN.md`](DOMAIN.md#claims-and-leases). Agent preflight uses
`GET /api/v1/status`. Without authorization it proves
server reachability. With a bearer token it also validates the identity and
returns that agent's live claims across projects. The store query uses the
existing owner/lease index and returns only the compact status shape; local
facts such as CLI version, config path, and `$TASKLINE_PROJECT` are composed by
the CLI. `POST /api/v1/agents/register` rejects a request that already carries
a valid token so an existing checkout identity cannot be replaced accidentally.

Canonical JSON fixtures in `testdata/http_contract/` guard the duplicated
HTTP shapes across server, CLI, and web tests. They are intentionally a
test-only drift net: they do not make the CLI import server packages, do
not introduce code generation, and do not change the runtime contract.
When adding or renaming a public JSON field, update the fixture and the
three local shape tests together.

Output formatting is centralized in `cli/internal/output`:

- `Resolve(flag)` picks JSON when stdout isn't a TTY (the default for
  agents), table otherwise.
- `Render` takes both a JSON value and a table-rendering closure so each
  command declares both shapes once.

## Configuration

Server config (`server/internal/config/config.go`) is environment
variables with optional `.env` overlay (process env wins). All paths
auto-`MkdirAll` on first boot:

- `TASKLINE_DB` — SQLite file (default `./data/taskline.db`)
- `TASKLINE_LISTEN` — listen addr (default `:8787`)
- `TASKLINE_IMAGES_DIR` — image storage root (default `./data/images`)
- `TASKLINE_DOCS_DIR` — markdown doc storage root (default `./data/docs`)

GitHub state verification reads `TASKLINE_GITHUB_TOKEN`, `GITHUB_TOKEN`, then
`GH_TOKEN`. When none is set it falls back to `gh auth token`, including common
Homebrew paths for LaunchAgent deployments. Tokens stay in memory and are not
written to taskline configuration or SQLite.

The checked-in `.env.example` intentionally points local runtime state at
ignored `./.cache/data/...`; the defaults above are what the server uses
when no `.env` value is present.

CLI config:

- `TASKLINE_SERVER` — base URL (default `http://127.0.0.1:8787`)
- `TASKLINE_PROJECT` — default `--project` value (so agents don't have
  to pass it on every subcommand)
- `.config/taskline/agent.json` — checkout-local agent id and bearer token;
  `taskline status` validates it before queue work

## Concurrency

`db.SetMaxOpenConns(1)`. SQLite under `modernc.org/sqlite` doesn't
reliably share PRAGMA state across connections, so we serialize access.
For a single-user local service that coordinates multiple agents, this is the
current correctness-over-throughput tradeoff. WAL is enabled so reads don't
block writes within that single connection's transaction queue.

If contention ever matters, lift the cap and move PRAGMA setup into a
connection initializer.

## Test strategy

- **Entrypoint**: the root `Makefile` is the canonical local, agent, and CI
  contract. `make check` runs lint, tests, release build, and skill validation;
  `MODULE=all|server|cli|web` narrows build, lint, and test targets.
- **Unit**: `service_test.go` and `store_test.go` cover happy paths and
  edge cases (cycle rejection, invalid-state rejection, idempotent dep
  insert). `:memory:` SQLite for speed.
- **End-to-end**: `server/tests/e2e_test.go` boots a real Hertz server
  on a random port and exercises the HTTP surface, including the SPA
  fallback. This is the regression net for handler ↔ service wiring.
- **HTTP contract drift guard**: `testdata/http_contract/` contains
  canonical JSON fixtures round-tripped by server model tests, CLI client
  tests, and web API-shape tests. This preserves module independence while
  making field drift visible in normal test runs.
- **CLI**: lives in the CLI module; uses an `httptest.Server` to fake
  the backend.
- **Web**: Vitest component tests, ESLint, and `pnpm build`.
- **Embedded Web contract**: `make test-server-bundle` builds the Vite
  production assets before compiling the complete server suite. With
  `TASKLINE_REQUIRE_WEB_BUNDLE=1`, the E2E test requires the embedded app shell
  and its local JavaScript entry asset, catching API-only or incomplete bundle
  builds. CI's server test matrix entry and `make check` use this path.
- **Browser E2E**: `scripts/test-browser.sh` owns the built server process,
  random port, temporary SQLite data, CLI seed manifest, and cleanup.
  `web/e2e/` uses Playwright with one Chromium worker for real pointer DnD,
  blocked dependency metadata, and Start/Pending creation semantics. Failed
  runs preserve Playwright and runtime evidence under stable ignored Web paths.
  `make test-browser`, `make check`, and CI share the same runner.
- **Seed fixture**: `scripts/seed.sh` composes only public CLI operations;
  `make test-seed` verifies its exact multi-state DAG against a real built
  server and CLI.
- **Skills**: `make test-skill` calls `scripts/test-skill.sh` to check public
  and internal `SKILL.md` frontmatter plus load-bearing section headings.
