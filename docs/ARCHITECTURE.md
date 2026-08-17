# Architecture

This document explains how BridgeCore is put together and, more usefully, *why*
each boundary sits where it does. The short version: a multi-tenant platform has
one invariant that matters more than all the others, and the architecture is
shaped around making that invariant hard to violate rather than easy to audit.

---

## 1. Layers

```
┌──────────────────────────────────────────────────────────────────┐
│ Transport                                                         │
│   internal/handler/*   REST handlers                              │
│   graph/resolver.go    GraphQL resolvers                          │
│   cmd/worker           export worker process                      │
│                                                                   │
│   Decode input. Delegate. Audit the outcome. Nothing else.        │
│   Holds: services. Does NOT hold: a database handle.              │
└───────────────────────────────┬──────────────────────────────────┘
                                │
┌───────────────────────────────▼──────────────────────────────────┐
│ Service — internal/service/*                                      │
│                                                                   │
│   Every business rule lives here exactly once: who may change     │
│   whose role, what a plan entitles, whether an export is allowed, │
│   and every tenancy guard.                                        │
│   Takes: tenancy.Scope. Never: a tenant id from a request body.   │
└───────────────────────────────┬──────────────────────────────────┘
                                │
┌───────────────────────────────▼──────────────────────────────────┐
│ Repository — internal/repository/*                                 │
│                                                                   │
│   All SQL. Tenant-scoped methods put the tenant in the WHERE      │
│   clause. Returns domain models and repository.ErrNotFound.       │
└───────────────────────────────┬──────────────────────────────────┘
                                │
                  PostgreSQL          Redis          S3 / filesystem
```

The rules are enforced by **what each layer is given**, not by convention:

- `graph.Resolver` has service fields and no `*sql.DB`. A resolver cannot reach
  the database except through business logic. This is not a guideline a reviewer
  has to check; it is a compile-time fact.
- Service methods take `tenancy.Scope` as a parameter. The compiler asks for it,
  so it cannot be forgotten.
- `handler` packages import `service`, never `repository`.

### Why three layers rather than two

A two-layer design (handler → repository) works until there are two transports.
The moment GraphQL arrives, every rule implemented in a handler has to be
reimplemented in a resolver, and the two drift. The drift is not hypothetical:
the interesting failure mode is a rule that exists in REST, is forgotten in
GraphQL, and is therefore *silently absent* from the newer, less-audited API.

With the service layer holding the rules, adding a transport adds no
authorization surface at all.

---

## 2. Request lifecycle

A REST request and a GraphQL request traverse the identical chain:

```
1.  RequestID          generate or sanitize the correlation id
2.  Recovery           catch panics; log with the correlation id; return 500
3.  SecurityHeaders    nosniff, DENY framing, no-referrer
4.  CORS               config-driven allow-list, not "*"
5.  Logging            one structured line per request
       ── per-route from here ──
6.  Auth               Bearer JWT or X-API-Key → AuthContext; re-reads the tenant
7.  RateLimit          Redis counter, keyed per tenant (hence after Auth)
8.  UsageMetering      records the request asynchronously
9.  RequireRole        admin > developer > viewer
10. RequireFeature     plan entitlement check
       ── handler or resolver ──
11. Service            business rules + tenancy guard
12. Repository         tenant-scoped SQL
```

Ordering choices worth stating:

**Auth before RateLimit.** The limiter keys on tenant id, which does not exist
until the credential is verified. Rate limiting by IP instead would punish an
entire office NAT for one noisy client, and would let a single tenant with many
egress IPs bypass the limit entirely.

**Auth re-reads the tenant row on every request.** A JWT is valid for its whole
TTL. If the tenant were trusted from the token, a tenant suspended five minutes
ago would keep working until every outstanding token expired. Reading the row
makes suspension and plan changes take effect on the next request. The cost is
one indexed point read per request, which is the right trade.

**Health probes bypass the chain entirely.** `/live`, `/ready` and `/health` are
never authenticated, rate limited, or metered. A probe that can be rate limited
will eventually be rate limited, and the orchestrator will conclude the service
is down and restart every task.

---

## 3. Tenant isolation

The invariant:

> No request authenticated for tenant A can read or modify any row belonging to
> tenant B, over any transport, through any endpoint.

Three independent layers enforce it, so a mistake at one is caught by another.

### 3.1 SQL

```sql
-- Tenant-scoped read
SELECT id, email, role, is_active FROM users WHERE id = $1 AND tenant_id = $2
```

Not `WHERE id = $1` followed by `if user.TenantID != scope.TenantID { ... }`.
The distinction matters for two reasons. First, the Go version loads the row
before rejecting it, so a *missing* check leaks data rather than merely failing
to double-check it. Second, anything added between the load and the check — a
log line, a metric, an error message including the row — leaks it even when the
check is present.

`internal/repository` therefore exposes explicitly named scoped methods:
`GetByIDInTenant`, `UpdateRoleInTenant`, `DeactivateInTenant`. The name makes an
unscoped call visible at the call site.

### 3.2 Service

```go
func (s *UserService) Get(ctx context.Context, scope tenancy.Scope, id string) (*models.User, error) {
    if err := scope.Require(); err != nil {
        return nil, err
    }
    user, err := s.repo.GetByIDInTenant(ctx, scope.TenantID, id)
    ...
}
```

`internal/tenancy` is deliberately tiny — a `Scope`, a `Guard`, an
`ErrCrossTenant`. Its value is that it is a *single named thing* to look for.
"Does this method take a Scope?" is a question with a mechanical answer.

### 3.3 Transport

The scope is derived only from the verified credential:

```go
func ScopeFromContext(ctx context.Context) tenancy.Scope
```

There is no exported function anywhere that builds a `Scope` from a request
body, a query parameter, or a path variable. This is the fix for the class of
bug that produced four of the five isolation defects in the original codebase:
an endpoint that accepts an ID from the caller and trusts it.

### 3.4 Why 404 and not 403

A 403 means "this exists, but not for you". That is an existence oracle: given a
403/404 distinction, an attacker enumerates a competitor's tenant and user IDs
by observing which responses differ. A 404 is indistinguishable from a
nonexistent ID.

The cost is debuggability for legitimate users who mistype an ID. It is paid
back by recording every denial as a `security.cross_tenant_denied` audit event
including the requested ID, so support can answer "what did I actually ask
for?", and by a CloudWatch metric filter that alarms on a burst of them. The
attacker's view is uninformative by design; the operator's is not.

---

## 4. The platform control plane

Cross-tenant operations live under `/api/v1/platform/*`, authenticated by
`X-Platform-Token` (constant-time compare, 404 on failure).

The reasoning is that `admin` cannot mean two things. If it means both
"administers their own tenant" and "administers the platform", then every
cross-tenant endpoint is one forgotten check away from being self-service. That
is exactly what happened to `POST /features/grant`, which trusted a `tenant_id`
from the request body and so let any tenant admin grant itself Enterprise
features.

With the split there is no request a customer credential can make that reaches a
cross-tenant operation — the property holds structurally rather than per
endpoint. The routes are not even registered when no operator token is
configured.

For the same reason, **plan is not a self-service field.** The plan determines
feature entitlements, so a writable plan is privilege escalation with extra
steps. `UpdateTenantSelfInput` has no `Plan` field, and neither does the
GraphQL `UpdateTenantInput`. Absence is stronger than validation: there is no
field to forget to check.

---

## 5. GraphQL as a transport

```go
type Resolver struct {
    Users        *service.UserService
    Tenants      *service.TenantService
    Entitlements *service.EntitlementService
    APIKeys      *service.APIKeyService
    Usage        *service.UsageService
    Audit        *service.AuditService
    Exports      *service.ExportService
    TenantSource TenantSource   // for the DataLoader only
}
```

No database handle, no repository (`TenantSource` is a narrow read interface for
batch loading). Every tenancy guard, RBAC rule and validation that REST enforces
applies here because there is no other path to the data.

### 5.1 Authorization

Declared per field, as decorators applied where the schema is built:

```go
"updateUserRole": &graphql.Field{
    Resolve: r.requiresRole(models.RoleAdmin, r.updateUserRole),
},
"requestUsageExport": &graphql.Field{
    Resolve: r.requiresFeature("usage.export", r.requestUsageExport),
},
```

These call `middleware.RoleAtLeast` and the same `EntitlementService` the REST
middleware calls. One role ordering, one entitlement resolver, one set of audit
records. The equivalent SDL directives are documented in
`graph/schema/directives.graphqls`; they are not executed, because executing the
Go helpers directly is what guarantees the two transports cannot diverge.

A field missing its decorator is visible in one file rather than requiring a
schema-wide audit.

### 5.2 Query cost limiting

A REST route has a fixed cost. One GraphQL document can request a page of users,
each with their tenant, each with its users — cheap to send, expensive to serve.

`graph/complexity.go` scans the **raw document before parsing**:

- **Depth** — nesting of selection sets.
- **Cost** — for each field, the product of the pagination multipliers of its
  enclosing selection sets. A page of 100 users costs 100; each field inside
  those users costs another 100.
- **Size** — bytes, checked first, so a hostile document is rejected before
  anything larger than the request body is allocated.
- **Introspection** — `__schema`/`__type` refused in production.

A variable page size (`pageSize: $n`) is costed at the configured maximum,
because the value is unknown until execution and a limiter that assumes the best
case is not a limiter. Resolvers then clamp `pageSize` to the same maximum,
which is what makes the worst-case assumption true rather than merely
pessimistic.

Rejections are audited as `graphql.rejected` with the measured depth and cost, so
a client hitting the ceiling repeatedly is visible rather than silently failing.

### 5.3 DataLoader

`users(pageSize: 100) { tenant { name } }` naively issues 1 + 100 queries.

`graph/dataloader.go` uses **prime-then-load**: the parent resolver already knows
every key its children will ask for, so it issues one `WHERE id = ANY($1)` and
primes a request-scoped cache. Children hit the cache. A key that was not primed
still resolves via a single fetch, so correctness never depends on the
optimisation having been applied — an unprimed path is slower, not wrong.

Deferred dispatch (the JavaScript DataLoader model) is more general, but it
requires the executor to resolve sibling fields concurrently so a batch window
can fill. If it does not, a resolver blocks forever on a batch that will never
fill. Prime-then-load achieves the same query count with no concurrency
assumptions and degrades safely.

Loaders are **per request**, never global. A shared cache of tenant rows would
serve one request a row another loaded — including serving a suspended tenant as
active.

### 5.4 What GraphQL deliberately does not do

Authentication stays REST-only. Login, signup, refresh and logout need precise
HTTP status codes, per-outcome rate-limit accounting, and trivially greppable
access logs. GraphQL's single-endpoint, always-200 model expresses none of those
well. GraphQL clients present the access token those endpoints issue, validated
by the same JWT middleware.

Queries are **POST-only**. A query in a URL lands in access logs, proxy caches
and browser history, and a mutation over GET is trivially CSRF-able. `GET
/graphql` serves the GraphiQL IDE in non-production only.

---

## 6. The export pipeline

The original endpoint streamed CSV while synchronously paging the usage table.
Three problems in production: a large tenant's export outlives the load
balancer's idle timeout; a retry repeats all the work; the request pins a task
for its whole duration.

```
POST /api/v1/usage/exports
        │
        ├─ INSERT export_jobs (status='queued')      ← durable, source of truth
        └─ SQS SendMessage {job_id, tenant_id}       ← best effort, a hint only
                    │
        ┌───────────┴───────────┬────────────────────┐
        ▼                       ▼                    ▼
  in-process worker     cmd/worker (ECS)     cmd/lambda-export
        └───────────────────────┴────────────────────┘
                                │
                    ClaimQueued: CTE + FOR UPDATE SKIP LOCKED
                                │
                    stream rows → temp file → object store
                                │
                    MarkCompleted(rows, bytes, key)
```

**PostgreSQL is the source of truth; SQS is a notification.** The queue carries a
*pointer* to the job, not the work. If SQS is unavailable or a message is lost,
the worker's next poll finds the job: the export is late, not lost. Making the
queue authoritative would trade a `SKIP LOCKED` query for a distributed
consistency problem plus job state that cannot be inspected with SQL.

**`FOR UPDATE SKIP LOCKED` means any number of consumers can run without
coordination.** Two workers claiming concurrently do not collide; the second
skips the locked row. This is why the ECS worker and the Lambda are safe to run
simultaneously, and why scaling the worker service needs no leader election.

**Rows stream to a temp file, then upload.** Memory does not scale with row
count. A 500,000-row export uses the same memory as a 500-row one.

**Stale reclamation.** A worker killed mid-export leaves a row in `processing`.
`ReleaseStale` returns rows whose claim is older than the visibility timeout to
`queued`, bounded by an attempt budget so a permanently failing job is marked
`failed` rather than retried forever.

**Download URLs expire and are never stored.** S3 presigned URLs in production;
HMAC-signed relative URLs against the local backend, where the signature covers
both the object key and the expiry so neither can be edited to reach another
tenant's export or to extend the window. Minting per request means a replayed
old response is not a usable capability.

---

## 7. Error contract

`pkg/apierr` defines a typed error with a stable machine-readable code. Every
error the API returns passes through `apierr.From`, which is the choke point
where an unrecognised error becomes a generic 500 rather than leaking a driver
message.

```
BAD_REQUEST  VALIDATION_FAILED  UNAUTHENTICATED  FORBIDDEN  NOT_FOUND
CONFLICT  RATE_LIMITED  FEATURE_NOT_ENTITLED  QUERY_REJECTED
INTERNAL_ERROR  SERVICE_UNAVAILABLE
```

`apierr.Error` implements `Extensions() map[string]interface{}`, which is the
GraphQL extended-error contract. So the same code appears in the REST envelope's
`error.code` and in the GraphQL `errors[].extensions.code`. A client that already
handles `FEATURE_NOT_ENTITLED` from REST handles it unchanged over GraphQL — the
two transports cannot develop separate error vocabularies.

Every response carries the request's correlation ID, so a user can quote one and
an operator can find the exact request in CloudWatch Logs.

---

## 8. Audit logging

Audit records are written as a side effect of the action they describe, never by
a caller-facing endpoint. There is no mutation and no `POST /audit`.

The table is **append-only at the database level**: a plpgsql trigger rejects
`UPDATE` and `DELETE` on `audit_logs`. An audit trail the application can rewrite
is not an audit trail, and the application is the component most likely to be
compromised. Enforcing it in the database means even a full application
compromise cannot quietly erase the evidence.

Notable events: `security.cross_tenant_denied` (isolation held, someone probed),
`feature.granted` / `feature.revoked` (a plan boundary moved),
`graphql.rejected` (a document exceeded the cost ceiling), plus the usual
authentication, role-change, API-key and export lifecycle events.

---

## 9. Configuration and startup

`config.Validate()` refuses to start in production when a JWT secret is a
default, is under 32 characters, or the access and refresh secrets are identical
(a shared secret means a refresh token can be replayed as an access token); when
CORS is `*`; when `sslmode=disable`; when the database password is a default; or
when GraphQL introspection or the playground is enabled.

A process that cannot be configured safely should fail loudly at boot rather
than serve traffic in a degraded security posture. Fifteen seconds of downtime
on a bad deploy is strictly better than an indefinite window with a guessable
signing key.

Secrets come from a `.env` file locally and from AWS Secrets Manager in
production, loaded *before* the environment is read (`cmd/api/bootstrap.go`), so
the rest of the process cannot tell the difference and the ECS task definition
contains no credentials at all.

Startup order: load secrets → load and validate config → connect Postgres (with
retry, because Compose and RDS failover both start the API before the database
is ready) → migrate → connect Redis → build repositories, services, handlers,
schema → register routes → start the worker → serve. Shutdown reverses it:
signal → stop claiming new export jobs → drain HTTP with a 20s timeout.

Exactly one component runs migrations — the API, on boot. The worker explicitly
does not, because two migrators racing during a rolling deploy is how a
half-applied migration happens.

---

## 10. Where the seams are

If you extend this codebase, these are the intended extension points:

| Want to… | Do this |
| --- | --- |
| Add an endpoint | Add a service method taking `tenancy.Scope`; add a handler and a resolver field that both call it. |
| Add a gated feature | Add the key to the catalog; wrap the route in `RequireFeature` and the field in `requiresFeature`. |
| Add a cross-tenant operation | Put it on `PlatformHandler`, behind the operator token. Never on a tenant-facing route. |
| Add a background job type | Follow `internal/exports`: a job table with `SKIP LOCKED` claiming, plus a notifier. |
| Change a limit | `internal/config`. Nothing reads a limit from a constant. |
| Add an object store backend | Implement `exports.ObjectStore` (three methods). |
