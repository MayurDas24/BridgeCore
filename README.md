# BridgeCore

A production-grade multi-tenant SaaS backend platform in Go: authentication,
RBAC, tenant isolation, plan-based feature entitlements, API keys, usage
metering, audit logging, rate limiting, and asynchronous usage exports —
exposed over **both REST and GraphQL**, deployed to **AWS ECS Fargate** with
**Terraform** and **GitHub Actions**.

The interesting problem in a platform like this is not any single feature. It is
that a mistake in the tenant boundary shows one customer another customer's
data, and there are dozens of places to make that mistake. Most of the design
decisions below exist to make that class of bug hard to write rather than easy
to review.

---

## Contents

- [Quick start](#quick-start)
- [Architecture](#architecture)
- [Tenant isolation](#tenant-isolation-the-core-invariant)
- [REST and GraphQL over one service layer](#rest-and-graphql-over-one-service-layer)
- [Asynchronous usage exports](#asynchronous-usage-exports)
- [The platform control plane](#the-platform-control-plane)
- [AWS deployment](#aws-deployment)
- [CI/CD](#cicd)
- [Testing](#testing)
- [Configuration](#configuration)
- [API reference](#api-reference)
- [Notable decisions and trade-offs](#notable-decisions-and-trade-offs)
- [Project layout](#project-layout)

---

## Quick start

**Requirements:** Go 1.22+, Docker with Compose. (Terraform 1.6+ only if you
want to deploy to AWS.)

```bash
# 1. Resolve dependencies. Required on a fresh clone: go.sum is not committed,
#    so this generates it and verifies the module graph.
go mod tidy

# 2. Format and build. Do this once before anything else — see the note below.
make fmt
go build ./...

# 3. Start Postgres, Redis, the API and the export worker, then seed demo data.
make up

# 4. Check it's alive.
curl -s localhost:8080/health | jq
```

Then:

| What | Where |
| --- | --- |
| REST API | `http://localhost:8080/api/v1` |
| GraphQL + GraphiQL IDE | `http://localhost:8080/graphql` |
| Swagger UI | `http://localhost:8080/docs` |
| Health / readiness / liveness | `/health`, `/ready`, `/live` |

Seeded logins (all with password `Password123!`):

| Email | Tenant plan | Role |
| --- | --- | --- |
| `admin@acme.test` | enterprise | admin |
| `dev@acme.test` | enterprise | developer |
| `viewer@acme.test` | enterprise | viewer |
| `admin@globex.test` | pro | admin |
| `admin@initech.test` | free | admin |

```bash
# Log in and keep the access token.
TOKEN=$(curl -s localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@acme.test","password":"Password123!"}' \
  | jq -r .data.tokens.access_token)

# REST
curl -s localhost:8080/api/v1/users -H "Authorization: Bearer $TOKEN" | jq

# The same data over GraphQL, through the same service layer
curl -s localhost:8080/graphql -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"query":"{ me { email role } tenant { name plan } users(pageSize:5){ nodes{ email role tenant{name} } pageInfo{ totalCount } } }"}' | jq
```

Without Docker, run `make run` (and `make run-worker` in a second terminal)
against a local Postgres and Redis configured in `.env` — start from
`cp .env.example .env`.

> **Please run `make fmt` and `go build ./...` first.** This codebase was
> authored in an environment with no Go toolchain and no module proxy, so
> nothing here has been compiled or gofmt'd. Expect to fix a small number of
> mechanical compile errors (an unused import, a renamed field) on first build.
> Everything is written against the real APIs of its dependencies, but "written
> correctly" and "verified by a compiler" are not the same claim, and it would
> be dishonest to present them as one. The same applies to
> `cd infra/terraform && terraform fmt -recursive` before CI's format gate will
> pass.

---

## Architecture

```
                            ┌──────────────────┐
   REST client ────────────▶│  /api/v1/*       │
                            │  net/http mux    │
   GraphQL client ─────────▶│  /graphql        │
                            └────────┬─────────┘
                                     │
              ┌──────────────────────▼───────────────────────┐
              │  Middleware chain (identical for both)       │
              │  request ID → recovery → security headers →  │
              │  CORS → logging → auth → rate limit →        │
              │  usage metering → RBAC → entitlements        │
              └──────────────────────┬───────────────────────┘
                                     │
              ┌──────────────────────▼───────────────────────┐
              │  Service layer — all business rules,         │
              │  all tenancy guards, all validation          │
              └──────────────────────┬───────────────────────┘
                                     │
              ┌──────────────────────▼───────────────────────┐
              │  Repository layer — every query carries      │
              │  WHERE tenant_id = $1                       │
              └──────────┬──────────────────────┬───────────┘
                         │                      │
                   ┌─────▼─────┐          ┌─────▼─────┐
                   │ PostgreSQL│          │   Redis   │
                   └─────┬─────┘          └───────────┘
                         │
              ┌──────────▼───────────┐      ┌──────────────┐
              │ export_jobs queue    │─────▶│ Export worker│──▶ S3 / filesystem
              │ FOR UPDATE SKIP      │      │ (in-process, │
              │ LOCKED               │      │  ECS, or     │
              └──────────────────────┘      │  Lambda)     │
                                            └──────────────┘
```

Layer rules, enforced by what each layer is *given* rather than by convention:

- **Handlers and resolvers** decode, delegate, and audit. Neither holds a
  database handle.
- **Services** own every business rule. They take a `tenancy.Scope`, never a
  tenant ID from a request body.
- **Repositories** own SQL. Tenant-scoped methods put the tenant in the
  `WHERE` clause, not in a Go comparison afterwards.

Full detail in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

---

## Tenant isolation: the core invariant

> No request authenticated for tenant A can read or modify any row belonging to
> tenant B, over any transport, through any endpoint.

Three independent layers enforce it, so a mistake at one is caught by another:

**1. SQL.** Tenant-scoped reads filter in the query:

```sql
SELECT id, email, role FROM users WHERE id = $1 AND tenant_id = $2
```

Not `WHERE id = $1` followed by an `if user.TenantID != scope.TenantID` check in
Go. The difference matters: the Go version loads the row before rejecting it, so
a missing check leaks data, and a logging statement added between the load and
the check leaks it into the logs.

**2. Service.** Every method takes `tenancy.Scope` and guards with
`tenancy.Guard`, which returns `ErrCrossTenant`.

**3. Transport.** The scope is derived only from the verified credential
(`middleware.ScopeFromContext`). There is no code path that builds a scope from
user input.

**Cross-tenant access returns 404, not 403.** A 403 says "this exists, but not
for you" — that is an existence oracle, and it is enough to enumerate a
competitor's tenant and user IDs. A 404 is indistinguishable from a
nonexistent ID. Denials are recorded as `security.cross_tenant_denied` audit
events, and a CloudWatch metric filter alarms on a burst of them, because the
attacker's view is deliberately uninformative and the operator's should not be.

### Isolation bugs found in the pre-refactor codebase

This project began as a working REST-only service. Auditing it before adding
GraphQL turned up five isolation defects, listed here because they are more
instructive than the fixes:

| Endpoint | Defect |
| --- | --- |
| `GET /tenants/{id}` | No tenant scoping — any authenticated user could read any tenant. |
| `PUT`/`DELETE /tenants/{id}` | Any tenant admin could modify or delete another tenant. |
| `GET /tenants` | Returned every tenant on the platform. |
| `POST /features/grant` | Trusted `tenant_id` from the request body, so any tenant admin could grant itself Enterprise features. |
| `UpdateRole`, `APIKey.GetByID`, `Audit.GetByID` | Tenancy checked in Go after loading the row rather than in SQL. |

Four of the five are the same underlying mistake: an endpoint that takes an ID
from the caller and trusts it. That is why `tenancy.Scope` is now a parameter
on every service method rather than something each one looks up for itself —
the compiler asks for it, so it cannot be forgotten.

---

## REST and GraphQL over one service layer

GraphQL here is a second **transport**, not a second application:

```go
// graph.Resolver holds services. It has no *sql.DB and no repository.
type Resolver struct {
    Users        *service.UserService
    Tenants      *service.TenantService
    Entitlements *service.EntitlementService
    // ...
}
```

A resolver physically cannot bypass a tenancy guard or an RBAC rule, because
there is no other path to the data. Authorization is declared per field as
resolver decorators that call the *same* helpers the REST middleware uses
(`middleware.RoleAtLeast`, the entitlement service):

```go
"updateUserRole": &graphql.Field{
    Resolve: r.requiresRole(models.RoleAdmin, r.updateUserRole),
},
"requestUsageExport": &graphql.Field{
    Resolve: r.requiresFeature("usage.export", r.requestUsageExport),
},
```

The GraphQL endpoint sits behind the identical middleware chain as REST, so a
GraphQL request is rate-limited, metered, correlated and audited the same way.

**Query cost limits.** A REST endpoint has a fixed cost per route; one GraphQL
document can nest a page of users, each with their tenant, each with its users.
`graph/complexity.go` scans the raw document *before* parsing and rejects it on
depth, on cost (field count multiplied by the pagination arguments in scope), or
on size. A variable page size (`pageSize: $n`) is costed at the configured
maximum, because a limiter that assumes the best case is not a limiter.
Introspection is refused in production.

**DataLoader.** `users { tenant { name } }` would otherwise issue one query per
user. `graph/dataloader.go` uses prime-then-load: the parent resolver knows
every key its children will request, so it issues one `WHERE id = ANY($1)` and
primes the cache. Unprimed keys still resolve via a single fetch, so correctness
never depends on the optimisation having been applied. `dataloader_test.go`
asserts the query count, not just the result.

**Authentication stays REST-only.** Login, signup, refresh and logout need
precise status codes and per-outcome rate-limit accounting; GraphQL's
single-endpoint, always-200 model expresses neither well. GraphQL clients
present the access token those endpoints issue.

The SDL under [`graph/schema/`](graph/schema/) is the human-readable contract.
The executable schema is built in Go, so it is compiled and type-checked with
the service and there is no generated file to keep in sync — see
[notable decisions](#notable-decisions-and-trade-offs) for why that differs from
the original plan to use gqlgen.

---

## Asynchronous usage exports

The original export endpoint streamed CSV while paging the usage table
synchronously. That breaks in three ways in production: a large tenant's export
outlives the load balancer's idle timeout, a retry repeats all the work, and the
request pins a task for its whole duration.

The replacement:

```
POST /api/v1/usage/exports        → 202 + job id      (requires usage.export)
GET  /api/v1/usage/exports/{id}   → status, and a download URL when complete
GET  .../{id}/download            → a freshly minted, expiring URL
```

- **PostgreSQL is the source of truth.** Jobs are claimed with a CTE plus
  `FOR UPDATE SKIP LOCKED`, so any number of workers can run without
  coordination and without duplicating work.
- **SQS is a best-effort notification**, carrying a pointer to the job rather
  than the work. If the queue is down, the worker's next poll picks the job up:
  the export is late, not lost. Making the queue authoritative would mean
  reconciling two sources of truth on every failure.
- **Three interchangeable consumers**, all running the same `exports.Worker`:
  in-process (local), a dedicated ECS service (`cmd/worker`), or a Lambda
  (`cmd/lambda-export`).
- **Rows stream to a temp file** and are then uploaded, so memory does not scale
  with row count.
- **Download URLs expire.** S3 presigned URLs in production; HMAC-signed
  relative URLs locally, where the signature covers both the object key and the
  expiry so neither can be edited. URLs are minted per request and never
  stored, so a replayed old response is not a usable capability.
- **Stale jobs are reclaimed** after a visibility timeout, so a worker killed
  mid-export does not strand the job in `processing` forever.

---

## The platform control plane

Cross-tenant operations — provisioning a tenant, changing a plan, granting an
entitlement — live under `/api/v1/platform/*` behind a separate
`X-Platform-Token` header, compared in constant time, returning **404** rather
than 401 so the control plane does not advertise itself.

The reason is that `admin` cannot mean both "administers their own tenant" and
"administers the platform". When it does, every cross-tenant endpoint is one
forgotten role check away from being self-service — which is exactly the
`POST /features/grant` bug in the table above. With the split, there is no
request a customer credential can make that reaches a cross-tenant operation.

For the same reason **a tenant cannot change its own plan.** The plan determines
feature entitlements, so a self-service plan field is a privilege escalation
with extra steps. `UpdateTenantInput` has no `plan` field at all, in both
transports.

---

## AWS deployment

Terraform in [`infra/terraform/`](infra/terraform/), ~16 files, no registry
modules — the point is to show the resources and their relationships.

```
                    Internet
                       │
              ┌────────▼────────┐
              │  ALB (public)   │  TLS 1.2+, /ready health check
              └────────┬────────┘
                       │
        ┌──────────────▼──────────────┐   private subnets, no public IP
        │  ECS Fargate                │
        │  ├─ bridgecore-api    ×2–6  │   autoscaled on CPU + requests/target
        │  └─ bridgecore-worker ×1    │   same image, different entrypoint
        └───┬──────────┬──────────┬───┘
            │          │          │
     ┌──────▼───┐ ┌────▼────┐ ┌───▼────────────┐
     │ RDS      │ │ Elasti- │ │ S3 (private) + │
     │ Postgres │ │ Cache   │ │ SQS + DLQ      │
     └──────────┘ └─────────┘ └────────────────┘
            │
     Secrets Manager  ──▶ injected by the ECS agent, never in the task definition
```

Security posture, and the reasoning:

- **Security groups reference other security groups**, never CIDR blocks, so a
  rule stays correct when tasks are rescheduled and does not silently permit
  future unrelated workloads in the VPC.
- **RDS and ElastiCache sit in subnets with no internet route**, so
  `publicly_accessible = false` is belt-and-braces rather than the only control.
  TLS is forced by the RDS parameter group, not merely requested by the client.
- **Two IAM roles per task.** The execution role (used by the ECS agent before
  the container starts) can pull images and read exactly two secrets. The task
  role (used by the application) can write objects under one S3 prefix and use
  one queue. It deliberately lacks `s3:ListBucket`, so a compromised task cannot
  enumerate which tenants have exports.
- **`iam:PassRole` is scoped to two role ARNs.** With a wildcard it is account
  takeover: the pipeline could launch a task as any role.
- **Secrets are generated by Terraform once, then ignored** on subsequent
  applies — otherwise every `terraform apply` would rotate the JWT secrets and
  invalidate every issued token.
- **S3**: all four public-access blocks, encryption, a TLS-only bucket policy,
  and a lifecycle rule that expires exports. Exports are reproducible from the
  usage table, so retaining them only grows the bill and the blast radius.
- **The deployment circuit breaker** rolls back automatically if a new task set
  never becomes healthy — before the pipeline's own rollback step is reached.

Detail in [`docs/AWS.md`](docs/AWS.md) and
[`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md).

```bash
cd infra/terraform
terraform init
terraform plan  -var 'github_repository=you/bridgecore' -var 'alarm_email=you@example.com'
terraform apply -var 'github_repository=you/bridgecore' -var 'alarm_email=you@example.com'

terraform output -raw github_deploy_role_arn   # → AWS_DEPLOY_ROLE_ARN secret
terraform output -raw platform_admin_token     # → to call /api/v1/platform/*
```

---

## CI/CD

**[`ci.yml`](.github/workflows/ci.yml)** — six parallel jobs:

| Job | Gate |
| --- | --- |
| `static-analysis` | `go mod tidy` produces no diff, gofmt clean, `go vet`, builds with the `integration` tag |
| `unit-tests` | `go test -race`, plus a coverage report |
| `integration-tests` | Real Postgres and Redis services; starts the actual binary and runs the black-box suite against it |
| `docker-build` | Builds the image and **asserts there is no shell in it** |
| `terraform-validate` | `fmt -check`, `init -backend=false`, `validate` — no credentials, so a fork PR cannot touch infrastructure |
| `secret-scan` | Fails on a tracked `.env`, an `AKIA…` key, or a private key block |

`-race` is not optional here: usage metering writes from a goroutine per
request, the worker runs concurrently with the API, and the DataLoader cache is
shared across resolvers.

**[`deploy.yml`](.github/workflows/deploy.yml)** — on `main`:

1. Re-run tests (last cheap place to stop a bad build).
2. Assume an AWS role via **OIDC** — no long-lived AWS keys exist in GitHub.
   A stolen repository secret is usable until someone notices; a stolen OIDC
   token expires within the hour. The trust policy is scoped to
   `repo:owner/name:ref:refs/heads/main`; without that condition, *any*
   repository on GitHub could assume the role.
3. Build and push, tagged by commit SHA — never only `latest`, or "roll back to
   the previous build" has no meaning.
4. Deploy and wait for service stability.
5. Smoke-test `/health` and verify the *deployed* version matches the commit.
6. On any failure, roll back to the task definition captured *before* the
   update.

---

## Testing

```bash
make test-race     # unit tests under the race detector
make test-cover    # coverage report
make up            # start the stack...
make integration   # ...then the black-box suite against it
```

Unit tests cover the parts where a subtle bug is silent: tenancy guards, RBAC
rules (cannot change your own role, cannot demote the last admin), query
complexity analysis, the DataLoader's query count, SigV4 canonicalization, and
the export worker's retry, requeue, signature-tampering and path-traversal
behaviour.

`integration/api_test.go` is black-box, over HTTP, against the real binary —
because the bugs it exists to catch are *wiring* bugs. A unit test proves the
tenancy guard rejects a cross-tenant ID; only an end-to-end request proves the
route is actually wired to the guarded method. The most dangerous security bug
in a codebase like this is a correct check that nothing calls.

It asserts, among others: cross-tenant reads *and writes* return 404; listings
never contain another tenant's rows; a tenant admin cannot change its own plan;
a tenant JWT cannot reach the platform plane or grant itself a feature; refresh
tokens are single-use; API key listings contain no plaintext or hash; a
free-plan tenant is refused exports with `FEATURE_NOT_ENTITLED`; oversized page
sizes are clamped; expensive and over-deep GraphQL documents are rejected;
GraphQL refuses GET; and error responses never leak SQL or hostnames.

---

## Configuration

All configuration is environment variables, documented in
[`.env.example`](.env.example). Secrets come from a `.env` file locally and from
AWS Secrets Manager in production — same contract either way, so the
application never knows the difference.

`config.Validate()` **refuses to start in production** when:

- a JWT secret is still a default, is shorter than 32 characters, or the access
  and refresh secrets are identical (a shared secret means a refresh token can
  be replayed as an access token);
- `CORS_ALLOWED_ORIGINS` is `*`;
- `DB_SSLMODE=disable`;
- the database password is still the default;
- GraphQL introspection or the playground is enabled.

A process that cannot be configured safely should fail loudly at boot rather
than serve traffic in a degraded posture. Boot logs the configuration through
`cfg.Redacted()`, so it is greppable without printing secrets.

---

## API reference

- OpenAPI: [`docs/openapi.yaml`](docs/openapi.yaml), served at `/docs`
- Postman: [`api/BridgeCore.postman_collection.json`](api/)
- GraphQL SDL: [`graph/schema/`](graph/schema/), plus GraphiQL at `/graphql`

Every REST response uses one envelope, and every error carries a stable
machine-readable code (`VALIDATION_FAILED`, `FEATURE_NOT_ENTITLED`,
`QUERY_REJECTED`, …) plus the request's correlation ID. The GraphQL errors array
carries the identical codes in `extensions`, so a client that already handles
`FEATURE_NOT_ENTITLED` from REST handles it unchanged over GraphQL.

```json
{
  "success": false,
  "message": "the pro plan does not include this feature",
  "error": {
    "code": "FEATURE_NOT_ENTITLED",
    "message": "the pro plan does not include this feature",
    "details": { "feature": "usage.export" }
  },
  "request_id": "8f14e45f-ea2b-4c1f-9b3a-1d4c8e77a0b2"
}
```

---

## Notable decisions and trade-offs

**GraphQL: `graphql-go/graphql` rather than gqlgen.** gqlgen is the better
choice for a team: SDL-first, generated bindings, exhaustive coverage checks.
It also requires a codegen step whose output must match hand-written resolver
signatures exactly. This codebase was authored without a Go toolchain, so a
codegen step that could not be run — and generated code that could not be
verified — was the larger risk. Schema-as-code compiles with the service and
has no generation step at all. The SDL is retained as the published contract.
With a compiler available, gqlgen would be the right call.

**No AWS SDK.** S3, SQS and Secrets Manager access is hand-written SigV4
(`pkg/awssig`) over `net/http`. This keeps the dependency graph at six modules
and makes the request signing legible rather than opaque. The trade-off is real:
no automatic retries with jitter, no endpoint discovery, no IMDS fallback. For
three well-specified services it is a good trade; for a service using twenty
AWS APIs it would not be.

**Postgres as the job queue, SQS as a hint.** Covered above. The alternative —
SQS as the source of truth — trades a `SKIP LOCKED` query for a distributed
consistency problem, and job state that cannot be inspected with SQL.

**`net/http` routing, no framework.** Go 1.22's `ServeMux` handles
`METHOD /path/{param}` patterns natively. The routing table then *is* the
authorization model, readable in one file: which credential every endpoint
requires is visible without opening a handler.

**404 over 403 for cross-tenant access.** Costs some debuggability for
legitimate users who mistype an ID. Removes an existence oracle. Mitigated by
auditing every denial with the requested ID, so support can still answer "what
did I ask for?"

**Distroless runtime image.** No shell, no package manager, no busybox: if the
API is compromised there is nothing in the image to pivot with. The cost is that
`docker exec` debugging is impossible and ECS container health checks (which all
require a shell) cannot be used — health is judged by the ALB polling `/ready`,
which is the signal that actually gates traffic anyway. CI asserts the absence
of a shell so this cannot silently regress.

**Prime-then-load DataLoader rather than deferred dispatch.** Deferred dispatch
is more general but needs the executor to resolve sibling fields concurrently so
a batch window can fill; if it does not, a resolver waits forever. Prime-then-
load achieves the same query count with no concurrency assumptions, and degrades
to correct-but-slower rather than hanging.

**Audit logs are append-only at the database level.** A plpgsql trigger rejects
`UPDATE` and `DELETE` on `audit_logs`. An audit trail that the application can
rewrite is not an audit trail, and the application is the thing most likely to
be compromised.

**Known gaps.** No distributed tracing (structured logs carry correlation IDs,
but there is no OpenTelemetry span export). No read replica or connection
pooler, so write and read load share one instance. Refresh tokens are stored
hashed and rotated but not bound to a device or IP. Rate limiting is a fixed
window in Redis, not a sliding window, so a burst can straddle a boundary.

---

## Project layout

```
cmd/
  api/              HTTP server: config, wiring, routes, graceful shutdown
  worker/           standalone export worker (own ECS service)
  lambda-export/    export consumer on the Lambda custom runtime
  seed/             idempotent demo data
graph/
  schema.go         GraphQL types, enums, inputs
  resolver.go       resolvers — services only, no DB access
  directives.go     @requiresRole / @requiresFeature as resolver decorators
  complexity.go     pre-execution depth and cost limiting
  dataloader.go     generic batch loader
  handler.go        POST-only transport, limits, GraphiQL
  model/            API DTOs — no credential fields, by construction
  schema/           SDL: the published contract
internal/
  config/           env → typed config + production safety validation
  middleware/       auth, RBAC, entitlements, rate limit, metering, CORS, logging
  handler/          REST handlers (+ the platform control plane)
  service/          all business logic and tenancy guards
  repository/       SQL, tenant-scoped
  exports/          object store, notifier, worker
  cloud/            S3, SQS, Secrets Manager, ECS credential provider
  tenancy/          Scope and Guard — the isolation primitive
  database/         Postgres/Redis connections, embedded migrations
  models/           domain types
  logger/           structured zap logging
pkg/
  apierr/           typed errors with stable codes, shared by REST and GraphQL
  response/         the single REST envelope
  awssig/           SigV4 signing and presigning, stdlib only
  jwt/, hash/, utils/
migrations/         SQL migrations (mirrored into internal/database/migrations)
integration/        black-box HTTP suite (build tag: integration)
infra/terraform/    AWS infrastructure
.github/workflows/  CI and CD
docs/               architecture, security, deployment, GraphQL, AWS, OpenAPI
```

Run `make help` for every available target.
