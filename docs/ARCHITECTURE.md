# BridgeCore — Architecture

## 1. High-Level Architecture

BridgeCore is a single deployable service today, structured internally as
clean, layered modules so it can be split into independent microservices
later without a rewrite (see §7).

```
                         ┌─────────────────────┐
                         │      Clients         │
                         │ (products, scripts,  │
                         │  frontends, CI jobs)  │
                         └──────────┬───────────┘
                                    │ HTTPS
                                    ▼
                    ┌───────────────────────────────┐
                    │        cmd/api (main.go)       │
                    │   net/http.ServeMux router     │
                    └───────────────┬─────────────────┘
                                    │
      ┌─────────────────────────────┴───────────────────────────────┐
      │                     Middleware chain                         │
      │  RequestID → Recovery → CORS → Logging → Auth → RateLimit    │
      │  → UsageMetering → RBAC/Entitlement (per-route)               │
      └─────────────────────────────┬───────────────────────────────┘
                                    │
                    ┌───────────────┴────────────────┐
                    │      internal/handler            │
                    │  (decode → call service → envelope) │
                    └───────────────┬────────────────┘
                                    │
                    ┌───────────────┴────────────────┐
                    │      internal/service             │
                    │  (business rules, no SQL, no HTTP) │
                    └───────────────┬────────────────┘
                                    │
                    ┌───────────────┴────────────────┐
                    │     internal/repository            │
                    │  (hand-written SQL, database/sql)  │
                    └───────────────┬────────────────┘
                                    │
                     ┌──────────────┴───────────────┐
                     ▼                               ▼
              ┌─────────────┐                ┌──────────────┐
              │ PostgreSQL   │                │    Redis      │
              │ (system of   │                │ (rate limits, │
              │  record)     │                │  future cache)│
              └─────────────┘                └──────────────┘
```

**Why layered, not domain-folders-plus-layers.** The original brief listed
both domain packages (`auth/`, `tenant/`, `entitlement/`, ...) and
layer packages (`repository/`, `service/`, `handler/`, ...) at the same
level. Maintaining both would mean every entity lives in two places for no
functional benefit. BridgeCore keeps the layer packages — `models`,
`repository`, `service`, `handler`, `middleware` — and organizes *within*
each layer by domain (e.g. `repository/tenant_repository.go`,
`service/auth_service.go`). This is the more defensible structure for a
codebase this size and is what most production Go platform teams converge
on.

**Why `database/sql` + `lib/pq` instead of GORM, and stdlib `net/http`
instead of Gin.** This is a deliberate, disclosed substitution driven by
the build environment: the sandbox this was built in has no route to the
Go module proxy or to `golang.org`, only direct GitHub access. Gin and
GORM's dependency graphs pull in packages that live behind those blocked
hosts (`bytedance/sonic`'s assembler toolchain, `golang.org/x/*` vanity
imports that don't resolve without `golang.org` itself, etc.). Go 1.22's
stdlib router (`"METHOD /path/{param}"` patterns) covers everything Gin was
being used for here, and hand-written SQL in the repository layer gives
full control over query plans and indexes with a much smaller dependency
surface. In an environment with normal internet access, swapping back to
Gin/GORM would be a mechanical, low-risk change — the handler/service/
repository boundaries don't depend on either.

## 2. Request Flow

1. `net/http.ServeMux` matches method + path (Go 1.22 pattern routing).
2. Global middleware runs: request ID assignment, panic recovery, CORS,
   structured request logging (wraps the whole chain to capture final
   status/latency).
3. Route-specific middleware runs in order: `Auth` (resolves identity),
   `RateLimit` (Redis-backed, per-tenant), `UsageMetering` (records the
   request asynchronously), then any `RequireRole` / `RequireFeature`
   gates for that specific route.
4. The handler decodes the request, calls into a service method, and
   writes the standard JSON envelope (`{success, message, data, error}`).
5. Services contain business rules only — no `net/http`, no SQL — and call
   into repositories.
6. Repositories run parameterized SQL against Postgres via `database/sql`.

## 3. Authentication Flow

BridgeCore supports two credential types, resolved by the same `Auth`
middleware:

- **JWT (human users).** `POST /auth/login` verifies bcrypt(password),
  then issues a short-lived **access token** (15 min default) and a
  longer-lived **refresh token** (7 days default), both HS256 JWTs with a
  `jti` (unique ID), `iat`/`exp`/`nbf`, and a `use` claim (`access` or
  `refresh`) so one can never be presented as the other. The refresh
  token's SHA-256 hash (not the token itself) is persisted, so a database
  leak alone can't be replayed. `POST /auth/refresh` verifies the
  presented refresh token, looks up its hash, and **rotates**: the old
  refresh token is revoked and a brand-new pair is issued. `POST
  /auth/logout` revokes every active refresh token for the user
  (ends all sessions).
- **API keys (machine-to-machine).** `POST /apikeys` generates a key of
  the form `bc_live_<24 random bytes, base32>`. The plaintext is returned
  **exactly once**; only its SHA-256 hash and last four characters are
  stored thereafter. Requests present it via `X-API-Key`; the middleware
  bcrypt/SHA-256-compares it against active keys sharing the same prefix.

Why SHA-256 instead of bcrypt for tokens/keys: bcrypt has a hard 72-byte
input ceiling (a JWT easily exceeds this — this was actually caught by the
test suite during development, see the auth service tests) and its
deliberate slowness exists to blunt brute-forcing of low-entropy *human*
passwords. Refresh tokens and API keys are already high-entropy random
values, so a fast, deterministic SHA-256 digest is the correct tool, with
a constant-time comparison to avoid timing side-channels. Passwords
themselves still use bcrypt (cost 12).

## 4. Usage Metering Flow

Every authenticated request is measured by the `UsageMetering` middleware,
which wraps the handler, times it, and fires an **asynchronous** (`go
func`, 3s bounded context) insert into `usage_logs` after the response has
already been written to the client — so a slow metering write never adds
latency to the actual API response. Records capture tenant, endpoint,
method, status code, latency, and request ID. `GET /usage` and `GET
/usage/summary` (aggregated per-endpoint counts/error-rates/avg latency)
read this table, always scoped to the caller's own tenant.

## 5. RBAC Flow

Roles are ranked (`viewer` < `developer` < `admin`). `RequireRole(min)`
middleware checks the authenticated user's role against that rank —
`admin` satisfies any `developer` or `viewer` requirement. Every rejection
is recorded as a `feature.access_denied` audit event with the required vs.
actual role in its metadata. API keys authenticate at `developer`-level
machine permissions by convention (no human "role" applies to a machine
credential).

## 6. Feature Entitlement / Audit Flow

Entitlements are resolved by `EntitlementService.HasFeature`, which checks
(in order): an explicit `tenant_features` grant (support/ops can flip a
feature for one tenant regardless of plan) → falls back to the tenant's
plan defaults (`PlanFeatureDefaults` — Free/Pro/Enterprise each unlock
different feature keys). The `RequireFeature` middleware runs this check
**before** the handler executes, per the platform requirement — see
`GET /api/v1/usage/export`, which is deliberately gated behind
`usage.export` (Pro/Enterprise only) as a live demonstration: a Free-plan
tenant gets a 403 before any usage data is even queried.

Every security- or business-relevant action — signup, login (success and
failure), logout, tenant CRUD, role changes, API key lifecycle events,
feature-access denials, and unauthorized requests — writes an immutable
row to `audit_logs` via `AuditService.Record`, which is called inline from
handlers/middleware and never blocks or fails the primary request (errors
writing an audit record are logged, not propagated).

## 7. Scaling Strategy & Future Microservices Split

BridgeCore is intentionally a modular monolith today. The layer boundaries
were chosen so that each domain slice can be extracted into its own
service with minimal churn:

| Candidate service | What moves | Shared dependency |
|---|---|---|
| **auth-service** | `service/auth_service.go`, `pkg/jwt`, `users`/`refresh_tokens` tables | Needs to expose token verification (public keys or a verify RPC) to every other service |
| **tenant-service** | `service/tenant_service.go`, `tenants` table | Source of truth other services would call for plan/status |
| **entitlement-service** | `service/entitlement_service.go`, `features`/`tenant_features` tables | Read-heavy; a strong caching (Redis) candidate |
| **usage-service** | `service/usage_service.go`, `usage_logs` table | Write-heavy, append-only — a natural fit for a queue-backed ingestion pipeline |
| **audit-service** | `service/audit_service.go`, `audit_logs` table | Same as above; compliance-sensitive, likely needs stricter retention/immutability guarantees |

Splitting would follow the strangler pattern: extract one service behind
its existing Go interface, stand up gRPC or REST between them, and cut
over traffic incrementally — the `service` package boundaries were
designed as the seams.

### Horizontal Scaling

The API is stateless (all session state lives in Postgres/Redis, not in
process memory), so it scales horizontally behind a load balancer with no
sticky-session requirement. `Server.Shutdown` with a 15s drain window
means rolling deploys don't drop in-flight requests.

### Redis Usage

Currently used for fixed-window, per-tenant rate limiting (`INCR` +
`EXPIRE`, fails open if Redis is unavailable — availability of the core
API takes priority over the limiter itself). Natural next uses: caching
resolved entitlements (`TenantHasFeature` is a hot read), caching JWT
verification results is unnecessary (stateless), and session/refresh-token
denylisting for immediate revocation.

### Kafka Integration (future)

`usage_logs` and `audit_logs` are both append-only, high-volume, and
consumed by other systems (billing, SIEM/compliance export, analytics).
The next evolution is to have `UsageMetering`/`AuditService` publish to a
Kafka topic instead of (or in addition to) a direct DB write, with a
consumer group performing the actual Postgres insert (or routing to a
columnar store better suited to that access pattern, e.g. ClickHouse for
usage analytics). This decouples the hot request path from write
throughput on the primary OLTP database entirely.

### Kubernetes Deployment (future)

The API is already a single stateless binary with `/live` and `/ready`
endpoints mapped directly to liveness/readiness probes, and honors
`SIGTERM` for graceful shutdown — it's Kubernetes-ready as-is. A
production `Deployment` would add: a `HorizontalPodAutoscaler` on CPU/RPS,
`PodDisruptionBudget`, secrets mounted from a `Secret`/external-secrets
operator rather than plain env vars, and a `NetworkPolicy` restricting
egress to Postgres/Redis/Kafka only.
