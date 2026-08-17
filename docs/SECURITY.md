# Security

This document describes BridgeCore's threat model, the controls that address it,
and — where a control is partial — what it does not cover.

---

## 1. Threat model

The assets, in rough order of how badly a breach hurts:

1. **Another tenant's data.** The defining risk of a multi-tenant platform.
2. **Credentials** — password hashes, refresh tokens, API keys, JWT signing
   secrets.
3. **The platform operator credential**, which crosses every tenant boundary.
4. **Availability**, including one tenant degrading service for others.
5. **The audit trail**, which is the only record of what happened after a breach.

The adversaries assumed:

| Adversary | Capability |
| --- | --- |
| Unauthenticated internet | Reach the ALB. Enumerate endpoints. Send arbitrary bodies. |
| A legitimate tenant | A valid credential and full knowledge of the API. Wants another tenant's data, a feature it has not paid for, or a higher plan. |
| A compromised tenant user | A stolen JWT or API key for one tenant, at that user's role. |
| A compromised task | Code execution inside a running container. |
| A compromised CI pipeline | Whatever the deploy credential permits. |

The tenant adversary is the interesting one. It is authenticated, it is
*supposed* to be there, and it can call every endpoint legitimately. Almost every
design decision below exists because of it.

---

## 2. Authentication

Two credential types, resolved into one `AuthContext`:

**JWT access tokens** (`Authorization: Bearer …`), 15 minutes by default. HS256
with separate access and refresh secrets. Claims carry user id, tenant id, role
and issuer.

**API keys** (`X-API-Key`), for machine callers. Stored as a SHA-256 hash; the
plaintext is shown exactly once, at creation or rotation, and is not recoverable.
A key is looked up by an indexed prefix and then verified against the hash, so
the lookup does not require scanning. Keys authenticate a *tenant*, not a person,
and act at developer-level permissions — which is why `GET /users/me` and the
GraphQL `me` field return nothing for an API-key credential rather than
inventing a user.

### The tenant row is re-read on every request

A JWT is valid for its full TTL. If the tenant's state were trusted from the
token, a tenant suspended five minutes ago would keep working until every
outstanding token expired, and a plan downgrade would not take effect until then
either. `middleware.Auth` reads the tenant row per request and rejects inactive
tenants immediately. The cost is one indexed point read; the benefit is that
suspension actually suspends.

### Separate access and refresh secrets

`config.Validate` rejects identical secrets in production. If they match, a
refresh token — which is long-lived by design — verifies as an access token, so
every refresh token becomes a week-long API credential.

### Refresh token rotation

Refresh tokens are stored hashed and are **single use**: refreshing revokes the
presented token and issues a new pair. Replaying a used token fails, which is
what makes a stolen refresh token detectable and short-lived rather than a
persistent backdoor. `TestAuth_SignupLoginRefreshLogout` asserts the replay
fails.

**Gap:** tokens are not bound to a device or IP, so a stolen token is usable from
anywhere until it is rotated or logout revokes the family. Binding to a device
fingerprint is the natural next step.

### Passwords

bcrypt, with a cost appropriate for interactive login. `PasswordHash` carries a
`json:"-"` tag on the domain model, and — more importantly — the GraphQL and
API DTOs in `graph/model` are separate structs that have no hash field at all.
Exposure would require adding a field, not merely forgetting to remove one. An
integration test greps every response for `password_hash`, `passwordHash` and
`key_hash`.

---

## 3. Authorization

### RBAC

Three roles with a total ordering: `admin > developer > viewer`. Expressed as
`RoleAtLeast(role, minRole)` rather than exact-match lists, so a new endpoint
says what it needs rather than enumerating who is allowed.

`middleware.RoleAtLeast` is exported and used by both the REST middleware and the
GraphQL decorators. There is one ordering, not two.

Rules that live in `UserService` and therefore apply to both transports:

- The caller must be an admin to change any role.
- A caller cannot change **their own** role — that is either escalation or a
  self-inflicted lockout.
- The **last admin** of a tenant cannot be demoted or deactivated, which would
  leave the tenant unadministrable and require operator intervention.
- A cross-tenant target returns 404.

### Feature entitlements

A tenant's enabled features are the union of its plan defaults and explicit
per-tenant grants. The check runs in middleware (`RequireFeature`) and in the
GraphQL decorator (`requiresFeature`), both calling the same
`EntitlementService.HasFeature`.

Because the check is in the routing table, an unentitled tenant never reaches the
code that would do the work. A plan boundary is enforced by the route
declaration rather than by remembering to check inside each handler.

Denials return `403` with code `FEATURE_NOT_ENTITLED` and the feature key, so a
client can prompt an upgrade rather than showing a generic error.

### Plan is not writable by tenants

The plan determines entitlements, so a tenant able to write its own plan can
grant itself anything. The field is **absent** from `UpdateTenantSelfInput` and
from the GraphQL `UpdateTenantInput` — absence rather than validation, because
there is then no field to forget to check. Plan changes are a platform
operation.

---

## 4. Tenant isolation

Covered in depth in [ARCHITECTURE.md §3](ARCHITECTURE.md#3-tenant-isolation).
Summarised here as controls:

| Layer | Control |
| --- | --- |
| SQL | Scoped queries filter on `tenant_id` in the `WHERE` clause. Explicitly named methods (`GetByIDInTenant`) make an unscoped call visible at the call site. |
| Service | Every method takes `tenancy.Scope`; guards return `ErrCrossTenant`. |
| Transport | The scope is derived only from the verified credential. No exported function builds a `Scope` from user input. |
| Response | Cross-tenant access returns **404**, never 403, to avoid an existence oracle. |
| Detection | Every denial is audited as `security.cross_tenant_denied`; a CloudWatch metric filter alarms on a burst. |
| Test | `integration/api_test.go` asserts isolation for reads, writes, listings, and GraphQL. |

### Defects found in the original codebase

Recorded because the pattern is more instructive than the fixes. Four of five
were the same mistake — an endpoint accepting an ID from the caller and trusting
it:

| Endpoint | Defect |
| --- | --- |
| `GET /tenants/{id}` | No scoping: any authenticated user could read any tenant. |
| `PUT`/`DELETE /tenants/{id}` | Any tenant admin could modify or delete another tenant. |
| `GET /tenants` | Returned every tenant on the platform. |
| `POST /features/grant` | Trusted `tenant_id` from the body: any tenant admin could grant itself Enterprise features. |
| `UpdateRole`, `APIKey.GetByID`, `Audit.GetByID` | Tenancy checked in Go after loading the row rather than in SQL. |

---

## 5. Input handling

- **Body size capped at 1 MiB** before the decoder sees it. Without a cap, every
  JSON endpoint is an unauthenticated memory-exhaustion primitive.
- **`DisallowUnknownFields`.** A typo in a client payload fails loudly instead of
  being silently dropped, which is how "why didn't my update apply?" bugs happen.
- **Trailing content rejected.** A body of two concatenated JSON objects is
  ambiguous; silently honouring the first is how request-smuggling bugs start.
- **Parameterised queries everywhere.** No string concatenation into SQL.
- **Pagination clamped, never unbounded.** `page_size` is clamped to the
  configured maximum rather than rejected, so a client asking for 10,000 gets 100
  plus a meta block saying there are more pages. No single request can ask the
  database for an unbounded result set.
- **Time parameters are parsed strictly.** A malformed `from` is a 400, not
  silently "no lower bound" — otherwise a client typo becomes a full-table
  export.
- **SQLSTATE, not error strings.** Unique- and foreign-key violations are
  detected via `pq.Error` codes (`23505`, `23503`) rather than substring
  matching, which breaks on a locale change or a driver upgrade.

---

## 6. Availability and abuse

**Rate limiting.** A Redis counter keyed per tenant (after authentication, so the
key exists), returning `429` with `RATE_LIMITED` and `X-RateLimit-*` headers. Per
tenant rather than per IP: per-IP punishes a shared NAT and is bypassed by a
tenant with many egress addresses.

*Gap:* it is a fixed window, not a sliding one, so a burst can straddle a
boundary and briefly achieve twice the nominal rate.

**GraphQL cost limiting.** Depth, cost and size ceilings applied before
execution — see ARCHITECTURE §5.2. This is the GraphQL equivalent of pagination:
without it, one small document can request an enormous amount of work.

**Timeouts.** `ReadHeaderTimeout` (10s) specifically defends against Slowloris —
a client that opens a connection and dribbles headers forever. Read 30s, write
60s, idle 120s. The ALB idle timeout (65s) is set *above* the application's write
timeout so the application, not the load balancer, decides when a response has
taken too long.

**Asynchronous exports.** A large export cannot hold a connection or pin a task,
because no request ever does the work.

**Connection pool sizing is configuration.** RDS enforces a hard
`max_connections`; the pool ceiling multiplied by the task count must stay under
it, or a scale-out event causes new tasks to fail to connect. The export Lambda
has `reserved_concurrent_executions` capped for the same reason — an unbounded
Lambda fan-out would exhaust the database and take the API down with it.

**Recovery middleware.** A panic in one handler returns a 500 for that request
rather than killing the process and every in-flight request with it.

---

## 7. Secrets

| Environment | Source |
| --- | --- |
| Local | `.env` (gitignored), from `.env.example` |
| Production | AWS Secrets Manager, loaded before the environment is read |

The production consequence is worth stating plainly: **the ECS task definition
contains no credentials.** It carries the secret's ARN and a task role permitted
to read it. So a leaked task definition leaks nothing, a leaked image leaks
nothing, and rotating a secret does not require rebuilding or re-registering
anything.

RDS master credentials are managed by RDS itself
(`manage_master_user_password`), so Terraform never handles the value and it
never appears in state.

Terraform generates the JWT secrets, operator token and export signing key once,
then `ignore_changes` on the secret version — otherwise every `terraform apply`
would rotate the signing keys and invalidate every issued token.

**Boot-time refusals.** `config.Validate()` will not start production with a
default or short JWT secret, identical access/refresh secrets, wildcard CORS,
`sslmode=disable`, a default database password, or GraphQL
introspection/playground enabled.

**Repository hygiene.** `.env` and a stray `export.csv` were removed from the
original repository and a `.gitignore` added. CI fails the build if a `.env` is
tracked, if an `AKIA…` access key appears, or if a private key block appears.
Boot logs configuration through `cfg.Redacted()`.

---

## 8. Transport security

- **TLS 1.2 minimum** at the ALB (`ELBSecurityPolicy-TLS13-1-2-2021-06`). The
  policies still permitting TLS 1.0/1.1 exist for legacy browsers; a
  server-consumed API has none.
- **HTTP redirects to HTTPS** when a certificate is configured. Without one, only
  an HTTP listener exists — acceptable for a demo, never for real traffic, since
  bearer tokens over plaintext are readable by anything on the path. This is
  called out in the Terraform variable description.
- **Database TLS is forced by the RDS parameter group** (`rds.force_ssl`), not
  merely requested by the client. The client asking is a client-side control; the
  server requiring is a server-side one.
- **Redis in-transit encryption** enabled, which is why the application exposes
  `REDIS_TLS` — the client must be told to speak TLS or every command fails.
- **S3 bucket policy denies non-TLS requests.** Presigned URLs are capabilities;
  one fetched over plaintext HTTP is readable in transit.
- **CORS is an allow-list**, and the response sets `Vary: Origin` so a cache
  cannot serve one origin's response to another.

---

## 9. Infrastructure

- **Security groups reference security groups**, never CIDR blocks. "Allow 5432
  from the task security group" stays correct as tasks are rescheduled;
  "allow 5432 from 10.20.0.0/16" silently permits every future workload in the
  VPC.
- **RDS and ElastiCache have no internet route.** `publicly_accessible = false`
  is then belt-and-braces rather than the only control — a misconfigured security
  group cannot expose them because there is no path.
- **The database security group has no egress rules.** The database has no
  legitimate reason to originate a connection; denying it removes an
  exfiltration path.
- **Two IAM roles per task.** The execution role (ECS agent, before the container
  starts) can pull images and read exactly two secrets. The task role
  (application, at runtime) can write objects under one S3 prefix and use one
  queue. It deliberately lacks `s3:ListBucket`, so a compromised task cannot
  enumerate which tenants have exports — only touch keys it already knows.
- **`iam:PassRole` is scoped to two role ARNs** with an `iam:PassedToService`
  condition. With a wildcard it is account takeover: the pipeline could launch a
  task as any role.
- **Distroless runtime image.** No shell, no package manager, no busybox. A
  compromised API has nothing in the image to pivot with — it cannot spawn
  `/bin/sh` or fetch a payload, because neither exists. Runs as UID 65532. CI
  asserts the absence of a shell so this cannot silently regress. The cost is
  that `docker exec` debugging and ECS container health checks (all of which need
  a shell) are unavailable; health is judged by the ALB polling `/ready`, which
  is the signal that gates traffic anyway.
- **ECR tags are immutable** and scanned on push. A deployed tag always means the
  same bytes, so "roll back to that SHA" cannot pull a different image than the
  one that was tested.
- **All four S3 public-access blocks**, encryption at rest, and a lifecycle rule
  that expires exports. Exports are reproducible from the usage table, so
  retaining them only grows the bill and the blast radius of a future credential
  leak.

---

## 10. CI/CD

**No long-lived AWS keys exist in GitHub.** The pipeline assumes a role via OIDC:
GitHub presents a short-lived token, AWS validates it against a trust policy, and
returns credentials that expire within the hour. A stolen repository secret is
usable until someone notices; a stolen OIDC token is worthless almost
immediately.

The trust policy is scoped to
`repo:owner/name:ref:refs/heads/main` and the `production` environment. **Without
that `sub` condition, any GitHub repository in the world could assume the role** —
it is the single most important line in `iam.tf`.

The deploy role is further confined to this ECS cluster by an `ecs:cluster`
condition, so a compromised workflow cannot redeploy unrelated services in the
account.

Gates before anything ships: `go mod tidy` diff, gofmt, `go vet`, `go test
-race`, black-box integration tests against a real Postgres and Redis, an image
build that asserts no shell is present, Terraform validate, and a secret scan.

The deployment itself waits for service stability, smoke-tests `/health`,
verifies the deployed version matches the commit, and rolls back to the task
definition captured *before* the update on any failure. The ECS deployment
circuit breaker is a second, independent layer that reverts a task set that never
becomes healthy.

---

## 11. Audit and detection

Audit records are written as a side effect of the action, never by a
caller-facing endpoint, and the table is **append-only at the database level** — a
plpgsql trigger rejects `UPDATE` and `DELETE`. An audit trail the application can
rewrite is not an audit trail, and the application is the component most likely
to be compromised.

Events that matter for detection:

| Event | Why it matters |
| --- | --- |
| `security.cross_tenant_denied` | Isolation held, but someone is enumerating. Alarms at 20 in 5 minutes. |
| `feature.granted` / `feature.revoked` | A plan boundary moved. Should only ever come from an operator. |
| `graphql.rejected` | A client is probing the cost ceiling. |
| `auth.unauthorized_request` | Includes invalid platform-token attempts. |
| `role.changed` | Privilege change, with previous and new role. |
| `export.requested` / `export.downloaded` | Bulk data left the system. |

Every log line and every error envelope carries the correlation ID, so an
incident starts from a user-quoted ID rather than a timestamp guess.

---

## 12. Known gaps

Stated explicitly, because a security document that claims completeness is not
credible:

- **No distributed tracing.** Structured logs carry correlation IDs, but there is
  no OpenTelemetry span export, so cross-service latency attribution is manual.
- **Refresh tokens are not device-bound.** Rotation limits the window; binding
  would shrink it further.
- **Rate limiting is a fixed window.** A burst can straddle the boundary and
  briefly double the nominal rate.
- **No WAF.** An AWS WAF in front of the ALB would add managed rule sets for
  common injection and bot patterns.
- **No read replica or connection pooler.** Read and write load share one RDS
  instance; a heavy export competes with request traffic for I/O.
- **No automatic secret rotation.** Secrets Manager supports rotation schedules;
  none is configured, so rotation is a manual operation.
- **Audit logs are not shipped off-host.** They live in the same database as the
  data they describe. Streaming them to a separate account would survive a full
  database compromise.
- **No per-tenant encryption keys.** All tenants' data shares one encryption
  context, so encryption at rest does not add a tenant boundary.

---

## Reporting

This is a portfolio project and not operated as a service. If you find something
wrong with the reasoning above, an issue or a note is welcome — the design
rationale is the part most worth challenging.
