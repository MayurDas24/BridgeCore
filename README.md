# BridgeCore

A reusable, multi-tenant backend platform providing the core services every SaaS product ends up building anyway — authentication, authorization, feature entitlements, API keys, usage metering, and audit logging — as REST APIs, so downstream products integrate instead of reimplementing.

## Problem Statement

Every new product at a growing company re-implements the same handful of platform concerns: who is this user, what tenant do they belong to, what are they allowed to do, what plan are they on, how many requests have they made, and what did they just do that needs a paper trail. BridgeCore centralizes those concerns behind one API surface — the way an internal platform team would — so product teams build product instead of rebuilding auth for the fifth time.

## Features

- **Authentication** — signup (provisions a new tenant + admin user), login, logout, refresh-token rotation, current-user lookup. bcrypt password hashing, HS256 JWTs with unique `jti`s and strict access/refresh separation.
- **Multi-tenancy** — every resource is scoped to a tenant; one tenant can never see another's data.
- **RBAC** — `admin` / `developer` / `viewer` roles, enforced by middleware *before* the handler runs, with every denial audited.
- **Feature entitlements** — Free / Pro / Enterprise plan defaults plus per-tenant overrides. `RequireFeature` middleware blocks a request before it reaches the handler (demonstrated live by `GET /api/v1/usage/export`, gated behind the `usage.export` feature).
- **API keys** — `bc_live_...` format, generate/rotate/deactivate, hashed at rest, usable via `X-API-Key` on any authenticated endpoint.
- **Usage metering** — every request recorded asynchronously (tenant, endpoint, method, latency, status); queryable via `/usage` and `/usage/summary`.
- **Audit logs** — immutable trail of every security/business-relevant action: logins, tenant changes, role changes, API key lifecycle, access denials.
- **Health checks** — `/health`, `/ready`, `/live`, each reporting real Postgres/Redis connectivity, version, and uptime.
- **Middleware stack** — request ID correlation, panic recovery, structured logging, CORS, dual JWT/API-key auth, RBAC, entitlements, async usage metering, Redis-backed rate limiting (fails open).
- Graceful shutdown, connection pooling, pagination/filtering/search, self-migrating schema.

## Architecture

```
Client
  → Router (Go 1.22 net/http, method+pattern matching)
  → Middleware chain: RequestID → Recovery → CORS → Logging
       → Auth → RateLimit → UsageMetering → RBAC/Entitlement (per route)
  → Handler   (decode request → call service → format response)
  → Service   (business rules — no HTTP, no SQL)
  → Repository (hand-written SQL via database/sql)
  → PostgreSQL / Redis
```

Full write-up — request flow, auth flow, RBAC flow, entitlement flow, and a future microservices split — is in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

**Note on stack choices:** this uses Go 1.22's native `net/http` pattern router instead of Gin, and `database/sql` + `lib/pq` with hand-written SQL instead of GORM. Every platform requirement — JWT auth, RBAC, entitlements, API keys, usage metering, audit logs, rate limiting, Docker, structured logging, graceful shutdown — is implemented as specified; these two choices don't affect any of that.

## Folder Structure

```
bridgecore/
├── cmd/
│   ├── api/          # HTTP server entrypoint, route registration
│   └── seed/          # Idempotent baseline-data seeder
├── internal/
│   ├── config/         # Env-driven configuration
│   ├── database/       # Postgres/Redis connections, embedded migrations
│   ├── handler/         # HTTP handlers
│   ├── logger/           # Structured logging setup
│   ├── middleware/       # Auth, RBAC, entitlements, rate limit, metering
│   ├── models/             # Domain structs
│   ├── repository/         # Hand-written SQL data access
│   └── service/             # Business logic
├── pkg/
│   ├── jwt/                 # Access/refresh token issuance & verification
│   ├── response/             # Standard JSON envelope helpers
│   └── utils/                 # Password hashing, API key generation
├── migrations/                # SQL migrations
├── api/                        # Postman collection
├── docs/                        # OpenAPI spec, Swagger UI, architecture doc
├── scripts/                      # Dev convenience scripts
├── Dockerfile
├── docker-compose.yml
└── Makefile
```

## Database Schema

Nine tables, UUID primary keys, `created_at`/`updated_at` timestamps throughout, indexed on every foreign key and commonly-filtered column:

`tenants`, `users`, `features`, `tenant_features`, `api_keys`, `refresh_tokens`, `usage_logs`, `audit_logs`, plus an internal `schema_migrations` tracking table.

```
tenants ──┬──< users ──< refresh_tokens
          ├──< api_keys
          ├──< tenant_features >── features
          ├──< usage_logs
          └──< audit_logs >── users (actor_id)
```

Full DDL: [`migrations/0001_init.up.sql`](migrations/0001_init.up.sql).

## Getting Started

### Prerequisites

- Docker + Docker Compose (recommended path — no local Go/Postgres/Redis needed)
- *or* Go 1.22+ with your own local Postgres and Redis

### Run with Docker

```bash
docker compose up --build
```

This builds the images, starts Postgres and Redis with health checks, and starts the API (which self-migrates the schema on boot). In a second terminal, seed baseline data:

```bash
docker compose run --rm seed
```

The API is now on `http://localhost:8080`.

```bash
curl http://localhost:8080/health

curl -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@bridgecore.dev","password":"AdminPass123!"}'
```

Full interactive API docs: `http://localhost:8080/docs` (Swagger UI).

> **Port already in use?** Set a different port before building:
> ```bash
> echo "APP_PORT=8090" > .env
> ```

### Run locally (no Docker)

```bash
cp .env.example .env
# point .env at your local Postgres/Redis
go run ./cmd/api        # self-migrates on boot
go run ./cmd/seed        # in a second terminal, once the API is up
```

### Seeded accounts

| Email | Password | Role | Tenant Plan |
|---|---|---|---|
| `admin@bridgecore.dev` | `AdminPass123!` | admin | Pro |
| `developer@bridgecore.dev` | `DevPass123!` | developer | Pro |
| `viewer@bridgecore.dev` | `ViewerPass123!` | viewer | Pro |

## API Overview

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/api/v1/auth/signup` | — | Create tenant + admin user |
| POST | `/api/v1/auth/login` | — | Log in |
| POST | `/api/v1/auth/refresh` | — | Rotate token pair |
| POST | `/api/v1/auth/logout` | JWT | Revoke all sessions |
| GET | `/api/v1/auth/me` | JWT | Current user |
| POST / GET | `/api/v1/tenants` | JWT (admin) | Create / list tenants |
| GET / PUT / DELETE | `/api/v1/tenants/{id}` | JWT | Get / update / soft-delete |
| GET | `/api/v1/users` | JWT | List tenant's users |
| PATCH | `/api/v1/users/{id}/role` | JWT (admin) | Change RBAC role |
| GET | `/api/v1/features`, `/features/mine` | JWT | Feature catalog / tenant's features |
| POST | `/api/v1/features/grant` | JWT (admin) | Grant/revoke a feature |
| POST / GET | `/api/v1/apikeys` | JWT (dev+) | Generate / list keys |
| POST | `/api/v1/apikeys/{id}/rotate` | JWT (dev+) | Rotate a key |
| DELETE | `/api/v1/apikeys/{id}` | JWT (dev+) | Deactivate a key |
| GET | `/api/v1/usage`, `/usage/summary` | JWT or API key | Query usage |
| GET | `/api/v1/usage/export` | JWT/API key + `usage.export` feature | CSV export |
| GET | `/api/v1/audit`, `/audit/{id}` | JWT | Audit trail |
| GET | `/health`, `/ready`, `/live` | — | Health checks |

Full spec: [`docs/openapi.yaml`](docs/openapi.yaml). Postman collection: [`api/BridgeCore.postman_collection.json`](api/BridgeCore.postman_collection.json).

## Testing

```bash
make test             # go test ./...
make test-coverage     # with coverage report
make vet                # go vet ./...
```

Unit tests cover the auth service, tenant service, usage service, and JWT issuance/verification, using in-memory fakes — no live database required.

## Tech Stack

Go 1.22 · PostgreSQL · Redis · JWT (`golang-jwt`) · bcrypt/SHA-256 · Zap (structured logging) · Docker & Docker Compose · GitHub Actions CI

## Future Scope

- Split hot paths (usage/audit ingestion) into separate services as load diverges — see [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
- Kafka-backed ingestion to decouple write throughput from the primary database
- Redis caching for entitlement resolution
- Per-tenant custom roles beyond the three fixed RBAC levels
- SSO (SAML/OIDC) — already represented as a gate-able feature key, not yet implemented

## License

MIT
