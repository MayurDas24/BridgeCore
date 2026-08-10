<<<<<<< HEAD
# BridgeCore

A reusable, multi-tenant backend platform providing the core services every
SaaS product ends up building anyway — authentication, authorization,
feature entitlements, API keys, usage metering, and audit logging — as REST
APIs, so downstream products integrate instead of reimplementing.

## Problem Statement

Every new product at a growing company re-implements the same handful of
platform concerns: who is this user, what tenant do they belong to, what
are they allowed to do, what plan are they on, how many requests have they
made, and what did they just do that we need a paper trail for. BridgeCore
centralizes those concerns behind one API surface, the way an internal
platform team at a company like Stripe, Atlassian, or SolarWinds would, so
product teams build product instead of rebuilding auth for the fifth time.

## Architecture

BridgeCore is a single Go binary today, internally structured in clean,
testable layers (handler → service → repository) so any layer can be
mocked in tests and any domain slice can be extracted into its own
microservice later with minimal churn. Full detail, request-flow diagrams,
and the microservices-split plan live in **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)**.

**A note on the tech stack vs. a typical brief for this kind of project:**
this was built in a sandboxed environment with no route to the Go module
proxy or to `golang.org` — only direct GitHub access. Gin and GORM's
dependency graphs both hit blocked hosts, so this uses **Go 1.22's native
`net/http` pattern router** (`"METHOD /path/{param}"`) instead of Gin, and
**`database/sql` + `lib/pq` with hand-written SQL** instead of GORM. Every
other requirement — JWT auth, RBAC, entitlements, API keys, usage
metering, audit logs, Redis-backed rate limiting, Docker, structured
logging, graceful shutdown — is implemented as specified. Swapping back to
Gin/GORM in an environment with normal internet access would be a
mechanical change; it doesn't touch the service/repository boundaries.

## Folder Structure

```
bridgecore/
├── cmd/
│   ├── api/            # HTTP server entrypoint, route registration
│   └── seed/            # Idempotent baseline-data seeder
├── internal/
│   ├── config/           # Env-driven configuration
│   ├── database/         # Postgres/Redis connections, embedded migrations
│   ├── handler/           # HTTP handlers (thin — decode, call service, respond)
│   ├── logger/            # Zap structured logger setup
│   ├── middleware/        # Auth, RBAC, entitlements, rate limit, metering, etc.
│   ├── models/             # Domain structs
│   ├── repository/         # Hand-written SQL data access
│   └── service/             # Business logic, no HTTP/SQL awareness
├── pkg/
│   ├── jwt/                 # Access/refresh token issuance & verification
│   ├── response/             # Standard JSON envelope helpers
│   └── utils/                 # Password hashing, API key generation
├── migrations/                # Canonical SQL migrations (edit here)
├── api/                        # Postman collection
├── docs/                        # OpenAPI spec, Swagger UI, architecture doc
├── scripts/                      # Dev convenience scripts
├── .github/workflows/             # CI
├── Dockerfile
├── docker-compose.yml
└── Makefile
```

## Features

- **Authentication** — signup (provisions a new tenant + admin user),
  login, logout, refresh-token rotation, current-user lookup. bcrypt
  password hashing, HS256 JWTs with unique `jti`s and access/refresh
  separation enforced via a `use` claim.
- **Tenant management** — full CRUD, soft delete, search + pagination.
- **RBAC** — `admin` / `developer` / `viewer`, enforced by middleware
  before the handler runs, with every denial audited.
- **Feature entitlements** — Free / Pro / Enterprise plan defaults plus
  per-tenant overrides; `RequireFeature` middleware blocks the request
  before it reaches the handler (demonstrated live by `GET
  /api/v1/usage/export`, gated behind `usage.export`).
- **API keys** — `bc_live_...` format, generate/rotate/deactivate, SHA-256
  hashed at rest, usable via `X-API-Key` on any authenticated endpoint.
- **Usage metering** — every request recorded asynchronously (tenant,
  endpoint, method, latency, status, timestamp); `GET /usage` and `GET
  /usage/summary` for querying.
- **Audit logs** — immutable trail of every security/business-relevant
  action, with actor, tenant, event, metadata, endpoint, IP, user agent.
- **Health checks** — `/health`, `/ready`, `/live`, each reporting real
  Postgres/Redis connectivity, version, build time, and uptime.
- **Middleware stack** — request ID correlation, panic recovery,
  structured logging, CORS, JWT+API-key auth, RBAC, entitlements, async
  usage metering, Redis-backed rate limiting (fails open).
- **Bonus requirements delivered** — graceful shutdown, rate limiting,
  DB transactions in the migration runner, connection pooling, pagination,
  filtering/search, request correlation IDs.

## Database Schema

Nine tables, all UUID primary keys, all with `created_at`/`updated_at`
(soft-deletable tables also get `deleted_at`), indexed on every foreign
key and every commonly-filtered column:

`tenants`, `users`, `roles` *(modeled as a `role` enum-like column on
`users` rather than a separate join table, since BridgeCore's 3 roles are
platform-fixed, not tenant-configurable)*, `features`, `tenant_features`,
`api_keys`, `refresh_tokens`, `usage_logs`, `audit_logs`.

Full DDL: [migrations/0001_init.up.sql](migrations/0001_init.up.sql).

### ER Diagram

```
tenants ──┬──< users ──< refresh_tokens
          ├──< api_keys
          ├──< tenant_features >── features
          ├──< usage_logs
          └──< audit_logs >── users (actor_id)
```

## Setup

### Option A — Docker (recommended, matches the spec exactly)

```bash
docker compose up --build
```

This builds the API image, starts Postgres and Redis with health checks,
waits for both to be healthy, and starts the API (which self-migrates the
schema on boot). Then run the seeder once:

```bash
docker compose run --rm seed
```

(Or just `make docker-up`, which does both steps.)

The API is now at `http://localhost:8080`. Try:

```bash
curl http://localhost:8080/health
curl -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@bridgecore.dev","password":"AdminPass123!"}'
```

### Option B — Local Go toolchain

```bash
cp .env.example .env
# start your own local Postgres + Redis, matching .env
go run ./cmd/api        # self-migrates on boot
go run ./cmd/seed        # separate terminal, once the API is up
```

Or use the helper script: `./scripts/dev-up.sh`.

### Environment Variables

See [.env.example](.env.example) for the full list with defaults —
covers app port/env, Postgres connection + pool sizing, Redis, JWT
secrets/TTLs, the API key prefix, and the rate limit.

**Production note:** the app refuses to boot with `APP_ENV=production`
if the JWT secrets are still the checked-in dev defaults.

## Running Locally / Testing

```bash
make build           # compile ./bin/bridgecore-api and ./bin/bridgecore-seed
make test             # go test ./...
make test-coverage     # with a coverage report
make vet                # go vet ./...
make fmt                  # gofmt -w .
```

Unit tests cover the authentication service, JWT issuance/verification,
the usage service, and the tenant service, using in-memory fakes (no live
database required to run `make test`). Two real bugs were caught by this
suite during development and fixed:

1. Refresh tokens are JWTs (150–300+ bytes) but were originally
   bcrypt-hashed for storage — bcrypt has a hard **72-byte** input limit,
   so this silently broke. Fixed by switching token/API-key hashing to
   SHA-256 + constant-time comparison (bcrypt's deliberate slowness is for
   low-entropy human passwords; it isn't the right tool for already-random
   tokens).
2. Two tokens issued within the same second had identical `iat`/`exp`
   (second-granularity) and were byte-for-byte identical strings. Fixed by
   adding a `jti` (UUID) claim to guarantee uniqueness.

## API Endpoints

Full interactive documentation is served by the running API at
`GET /docs` (Swagger UI, backed by [docs/openapi.yaml](docs/openapi.yaml)).
A ready-to-import Postman collection is at
[api/BridgeCore.postman_collection.json](api/BridgeCore.postman_collection.json).

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/api/v1/auth/signup` | — | Create tenant + admin user |
| POST | `/api/v1/auth/login` | — | Log in |
| POST | `/api/v1/auth/refresh` | — | Rotate token pair |
| POST | `/api/v1/auth/logout` | JWT | Revoke all sessions |
| GET | `/api/v1/auth/me` | JWT | Current user |
| POST/GET | `/api/v1/tenants` | JWT (admin) | Create / list tenants |
| GET/PUT/DELETE | `/api/v1/tenants/{id}` | JWT | Get / update / soft-delete |
| GET | `/api/v1/users` | JWT | List tenant's users |
| PATCH | `/api/v1/users/{id}/role` | JWT (admin) | Change RBAC role |
| GET | `/api/v1/features` | JWT | Full feature catalog |
| GET | `/api/v1/features/mine` | JWT | Tenant's enabled features |
| POST | `/api/v1/features/grant` | JWT (admin) | Grant/revoke a feature |
| POST/GET | `/api/v1/apikeys` | JWT (dev+) | Generate / list keys |
| POST | `/api/v1/apikeys/{id}/rotate` | JWT (dev+) | Rotate a key |
| DELETE | `/api/v1/apikeys/{id}` | JWT (dev+) | Deactivate a key |
| GET | `/api/v1/usage`, `/usage/summary` | JWT or API key | Query usage |
| GET | `/api/v1/usage/export` | JWT or API key + `usage.export` feature | CSV export |
| GET | `/api/v1/audit`, `/audit/{id}` | JWT | Audit trail |
| GET | `/health`, `/ready`, `/live` | — | Health checks |

## Testing

See "Running Locally / Testing" above. CI (`.github/workflows/ci.yml`)
runs `go vet`, a `gofmt` check, the full test suite with `-race`, builds
both binaries, boots the API against real Postgres/Redis service
containers and asserts `/health` reports healthy, then builds the Docker
image.

## Future Scope

- Split into the microservices outlined in
  [docs/ARCHITECTURE.md, §7](docs/ARCHITECTURE.md)
  as load characteristics diverge (usage/audit ingestion in particular).
- Kafka-backed ingestion for `usage_logs`/`audit_logs` to decouple write
  throughput from the primary OLTP database.
- Redis caching for entitlement resolution (currently a DB round trip per
  request).
- Per-tenant custom roles / fine-grained permissions beyond the three
  fixed RBAC roles, if a product integrating with BridgeCore needs it.
- SSO (SAML/OIDC) — already represented as a gate-able feature
  (`sso.saml`) in the entitlement catalog, not yet implemented.
- Swap `net/http`/`database/sql` back to Gin/GORM if desired in an
  environment with full package-proxy access — the service/repository
  interfaces were designed so this is a contained, mechanical change.
=======
# BridgeCore
>>>>>>> 7dcf798a90f41e49194359d494e65b5be9ad15b7
