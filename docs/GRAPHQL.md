# GraphQL API

BridgeCore's GraphQL endpoint is a **second transport over the same service
layer**, not a second application. This document covers how to use it, and the
design constraints that shape it.

- Endpoint: `POST /graphql`
- IDE: `GET /graphql` (GraphiQL, non-production only)
- SDL contract: [`graph/schema/`](../graph/schema/)

---

## Authentication

Authentication is REST-only. Log in, then present the access token:

```bash
TOKEN=$(curl -s localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@acme.test","password":"Password123!"}' \
  | jq -r .data.tokens.access_token)

curl -s localhost:8080/graphql \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"query":"{ me { email role } tenant { name plan } }"}' | jq
```

An API key works too (`X-API-Key`), in which case `me` returns `null` — an API
key authenticates a *tenant*, not a person, so null is the truthful answer rather
than a fabricated user.

**Why not a `login` mutation?** Credential endpoints need precise HTTP status
codes, per-outcome rate-limit accounting, and access-log entries that are
trivially greppable. GraphQL's single-endpoint, always-200 model expresses none of
those well: every login attempt, successful or not, would be a 200 to the same
URL.

**POST only.** A query in a URL lands in access logs, proxy caches and browser
history, and a mutation over GET is trivially CSRF-able. `GET /graphql` serves the
IDE and nothing else.

---

## Queries

```graphql
query Dashboard {
  me { id email firstName lastName role }

  tenant { id name slug plan isActive }

  users(page: 1, pageSize: 20) {
    nodes {
      id
      email
      role
      isActive
      tenant { name plan }   # batch-loaded, one query for the whole page
    }
    pageInfo { page pageSize totalCount totalPages hasNext }
  }

  myFeatures                  # feature keys enabled for this tenant

  usageSummary(from: "2026-08-01T00:00:00Z") {
    endpoint
    method
    requestCount
    errorCount
    avgLatencyMs
  }
}
```

Available queries:

| Field | Notes |
| --- | --- |
| `me` | Null for API-key credentials. |
| `tenant` | Always the caller's own. There is deliberately no argument. |
| `users`, `user(id:)` | An ID from another tenant resolves to **null**, never to a different tenant's user. |
| `features`, `myFeatures` | Catalog, and this tenant's enabled keys. |
| `apiKeys` | Requires developer or above. No hash field exists on the type. |
| `usage`, `usageSummary` | Metered requests, filterable by endpoint/method/time. |
| `auditLogs` | Requires developer or above. |
| `exportJobs`, `exportJob(id:)`, `exportDownload(id:)` | Require the `usage.export` entitlement. |

## Mutations

```graphql
mutation {
  updateTenant(input: { name: "Acme Corporation" }) { id name }

  updateUserRole(input: { userId: "…", role: DEVELOPER }) { id email role }

  generateApiKey(input: { name: "ci-pipeline" }) {
    apiKey { id prefix lastFour createdAt }
    plaintext                # shown once, never recoverable
  }

  requestUsageExport(input: { from: "2026-08-01T00:00:00Z" }) {
    id status createdAt
  }
}
```

Note what is **absent** from `UpdateTenantInput`: there is no `plan` field. A
tenant's plan determines its feature entitlements, so a writable plan would be
privilege escalation with extra steps. Plan changes are a platform operation on
`/api/v1/platform/*`. Absence is stronger than validation — there is no field to
forget to check.

---

## Authorization

Declared per field, as resolver decorators applied where the schema is built:

```go
"updateUserRole": &graphql.Field{
    Resolve: r.requiresRole(models.RoleAdmin, r.updateUserRole),
},
"requestUsageExport": &graphql.Field{
    Resolve: r.requiresFeature("usage.export", r.requestUsageExport),
},
```

These call `middleware.RoleAtLeast` and the same `EntitlementService.HasFeature`
the REST middleware calls. One role ordering, one entitlement resolver, one set of
audit records — the two transports cannot drift into different authorization
models. The equivalent SDL directives are documented in
`graph/schema/directives.graphqls` for readers of the contract; they are not what
executes.

The resolver struct itself is the deeper control:

```go
type Resolver struct {
    Users   *service.UserService
    Tenants *service.TenantService
    // ... services only. No *sql.DB. No repository.
}
```

A resolver *cannot* bypass a tenancy guard, because there is no other path to the
data. Tenant isolation over GraphQL is not a parallel implementation to keep in
sync; it is the same code.

---

## Errors

GraphQL reports field-level failures in the `errors` array with HTTP 200 — that is
the specification, since a partial result is still a result. Codes match REST
exactly:

```json
{
  "data": { "exportJobs": null },
  "errors": [{
    "message": "the free plan does not include this feature",
    "extensions": {
      "code": "FEATURE_NOT_ENTITLED",
      "status": 403,
      "details": { "feature": "usage.export" }
    }
  }]
}
```

`apierr.Error` implements the GraphQL extended-error contract, so a client that
already handles `FEATURE_NOT_ENTITLED` from REST handles it unchanged here.
Execution errors are also logged server-side with the correlation ID, so a 200
carrying a 500 is not invisible in monitoring.

---

## Cost limits

A REST route has a fixed cost per request. One GraphQL document can request a page
of users, each with their tenant, each with its users — cheap to send, expensive
to serve. Documents are analysed **before** execution:

| Limit | Default | Env var |
| --- | --- | --- |
| Depth | 10 | `GRAPHQL_MAX_DEPTH` |
| Complexity | 500 | `GRAPHQL_MAX_COMPLEXITY` |
| Document size | 16 KiB | `GRAPHQL_MAX_QUERY_BYTES` |
| Introspection | off in production | `GRAPHQL_INTROSPECTION` |

**Cost** is the sum, over every field, of the product of the pagination
multipliers of its enclosing selection sets. So `users(pageSize: 100) { id email }`
costs about 200, and adding `tenant { name plan }` inside adds another 200. The
response includes `X-GraphQL-Complexity` so a client can see how close it is to
the ceiling.

A **variable** page size (`pageSize: $n`) is costed at the configured maximum,
because the value is unknown until execution and a limiter that assumes the best
case is not a limiter. Resolvers then clamp `pageSize` to that same maximum, which
is what makes the worst-case assumption true rather than merely pessimistic.

Size is checked first, so a hostile document is rejected before anything larger
than the request body is allocated. Rejections return `QUERY_REJECTED` with the
measured value and the limit, and are audited as `graphql.rejected` so a client
repeatedly hitting the ceiling is visible.

If a legitimate query is refused, request a smaller `pageSize` or select fewer
nested fields — or raise `GRAPHQL_MAX_COMPLEXITY` if the query really is
reasonable for your data volumes.

---

## The N+1 problem

`users(pageSize: 100) { tenant { name } }` naively issues 1 + 100 queries.
`graph/dataloader.go` reduces it to 2.

The strategy is **prime-then-load**: the `users` resolver already knows every
tenant ID its children will ask for, so it issues one `WHERE id = ANY($1)` and
primes a request-scoped cache. The `tenant` resolvers then hit the cache. A key
that was not primed still resolves via a single fetch, so correctness never
depends on the optimisation having been applied — an unprimed path is slower, not
wrong.

This differs from the deferred-dispatch model a JavaScript DataLoader uses.
Deferred dispatch is more general, but it requires the executor to resolve sibling
fields concurrently so a batch window can fill; if it does not, a resolver blocks
forever on a batch that will never fill. Prime-then-load achieves the same query
count with no concurrency assumptions.

Loaders are **per request**, never global. A shared cache of tenant rows would
serve one request a row another loaded — including serving a suspended tenant as
still active.

`graph/dataloader_test.go` asserts the query *count*, not just the result, so a
regression that reintroduces N+1 fails the test rather than merely being slower.

---

## Schema-as-code, and why not gqlgen

The spec for this project called for gqlgen, and for a team that is the better
choice: SDL-first, generated bindings, exhaustive coverage checks.

This implementation uses `graphql-go/graphql` and builds the schema in Go. The
reason is specific rather than ideological: this codebase was authored in an
environment with no Go toolchain, so a codegen step could not be run and its
output could not be verified. gqlgen has two failure modes that both require a
compiler to catch — generation itself, and resolver signatures matching generated
interfaces exactly. Schema-as-code has neither: the schema is ordinary Go that
compiles with the service, and a resolver that stops matching its field is a
compile error.

The SDL under `graph/schema/` is retained as the published contract — it is what
you hand a client team. It is documentation, not codegen input.

With a compiler available, migrating to gqlgen would be a contained change: the
resolver bodies already delegate to services and would move across nearly
verbatim.

---

## Things GraphQL deliberately does not expose

- **Credentials.** `graph/model` types are separate structs from the database
  models, so `PasswordHash` and `KeyHash` have nowhere to appear even by accident.
  An integration test greps every response for them.
- **Cross-tenant anything.** No query takes a tenant ID. `user(id:)` with another
  tenant's ID returns null.
- **Plan writes.** Covered above.
- **Audit writes.** Audit records are produced only as a side effect of the action
  they describe, and the table rejects `UPDATE`/`DELETE` at the database level.
- **Introspection in production**, so the schema does not publish itself to
  unauthenticated scanners.
