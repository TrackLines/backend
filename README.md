# Tracklines backend


The Tracklines HTTP API: projects, boards, columns, tickets, backlog, sprints, roadmaps,
comments, attachments, billing, invitations and the public roadmap/intake endpoints. It also
serves the [MCP](#mcp) endpoint and the webhooks for Clerk, Stripe and the Git integrations.

Go HTTP service backed by PostgreSQL 18 and Valkey. Clerk for auth, Stripe for subscriptions,
Bugfixes for error reporting, Flags.gg for feature flags.

## Organization onboarding (TL-705)

Human org admins use `GET /api/onboarding` to resume setup, `POST /api/onboarding/setup`
to atomically create a project/board or adopt an imported board, and `POST /api/onboarding/finish`
after optional invitations. Existing organizations skip setup. Project creation shares the plan
limit lock with normal creation and imports, and retries return the same saved board.

`POST /api/onboarding/guide/complete` separately records user, organization, guide ID/version and
completion time. Publish a new `GuideVersion` in `internal/onboarding/onboarding.go` only with
matching frontend guide content for new features. Routine releases never reset completion.
Migration 106 adds the two tables; user removal and organization purge clean up these records.

## Run it

```sh
go run ./cmd/server        # serve the API
go test ./...              # full suite (uses testcontainers; needs Docker)
go vet ./...
```

Or bring up the whole stack (API, frontend, Postgres, Valkey) from the repo root with
`docker compose up --build`. The API listens on `8080` locally (`HTTP_PORT`, or Railway's
injected `PORT`). Migrations run automatically before it serves, so a fresh database is fine.

## Layout

- `cmd/server/main.go` — wires dependencies and starts the service; also runs the Railway cron
  jobs (`purge`, `sweep-files`).
- `internal/service.go` — **every route is declared here** via `Handler()`, with its auth layer
  (`optional` / `required` / `signedIn` / `sessionRequired` / `public`).
- `internal/<pkg>/` — one package per domain (`boards`, `tickets`, `roadmaps`, `billing`,
  `embed`, `intake`, `mcp`, …). HTTP helpers in `httpx`, auth in `auth`, config in `config`.
- `internal/migrations/NNNNNN_name.{up,down}.sql` — schema migrations, applied in order.

## Config

Read via [`keloran/go-config`](https://github.com/keloran/go-config) from the environment — never
hand-rolled parsing. Key vars:

| Var | Purpose |
|-----|---------|
| `RDS_URL` | PostgreSQL connection |
| `VALKEY_URL` | Valkey (live views + caching; API still runs without it) |
| `CLERK_*`, `CLERK_WEBHOOK_SECRET`, `CLERK_AUTHORIZED_PARTIES` | authentication |
| `STRIPE_*`, `STRIPE_WEBHOOK_SECRET` | billing |
| `TRACKLINES_SECRET_KEY` | sealing integration secrets |
| `FLAGS_*` | Flags.gg project/agent/environment IDs |
| `PUBLIC_API_URL`, `PORTAL_URL` | public base URLs |
| `GITHUB_*`, `BITBUCKET_*` | Git integration apps |
| `BUGFIXES_*` | error reporting |

Providers can be left unset for local scaffolding; the SDKs no-op and feature flags return false.

## Tests

Every feature is tested, including org/admin scoping. Integration tests spin up Postgres and Valkey
via testcontainers (`internal/testdb`, `TEST_DATABASE_URL`), so Docker must be running.
`go test ./...` runs the whole suite — don't ship with it red.

## MCP

The API serves MCP at `/mcp` (Streamable HTTP, stateless). Auth is the same as REST:
`Authorization: Bearer tl_…` (an API key) or a Clerk session token. Each tool calls a REST route
in-process, so permissions, validation and errors match REST exactly (`internal/mcp`).

## API docs

The full HTTP API is in [`openapi.yaml`](openapi.yaml).
