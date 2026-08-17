# AWS architecture

What is provisioned, why each choice was made, and what the alternatives would
have cost. Terraform lives in [`infra/terraform/`](../infra/terraform/); the
operational side is in [DEPLOYMENT.md](DEPLOYMENT.md).

---

## 1. Topology

```
                            Internet
                               │
                    ┌──────────▼──────────┐
                    │  ALB (public subnets)│  TLS 1.2+, health check on /ready
                    └──────────┬──────────┘
                               │  only port 8080, only to the task SG
        ┌──────────────────────▼──────────────────────┐
        │  ECS Fargate — private subnets, no public IP │
        │    bridgecore-api      ×2–6  (autoscaled)    │
        │    bridgecore-worker   ×1    (same image)    │
        └───┬───────────┬────────────┬────────────┬───┘
            │           │            │            │
      ┌─────▼────┐ ┌────▼─────┐ ┌────▼─────┐ ┌────▼──────────┐
      │   RDS    │ │ Elasti-  │ │ S3       │ │ SQS + DLQ     │
      │ Postgres │ │ Cache    │ │ (private)│ │               │
      │ (private)│ │ (private)│ └────┬─────┘ └────┬──────────┘
      └──────────┘ └──────────┘      │            │
                                     │       ┌────▼──────────┐
      Secrets Manager ──────────────▶│       │ Lambda        │
      (injected by the ECS agent)    │       │ (optional)    │
                                     │       └───────────────┘
                              S3 gateway endpoint
                          (export uploads bypass NAT)
```

Two subnet tiers. Public holds only the ALB and the NAT gateway. Everything else
is private, and **RDS and ElastiCache sit in subnets with no route to an internet
gateway** — which is what makes `publicly_accessible = false` meaningful rather
than merely stated. A misconfigured security group cannot expose them, because
there is no path.

Subnets are carved from the VPC CIDR with `cidrsubnet`, offset so adding an AZ
never renumbers existing subnets (which would force a replace of everything in
them).

---

## 2. Compute: ECS Fargate

Fargate rather than EC2: no instance patching, no capacity planning, no cluster
autoscaler. The trade-off is a higher per-vCPU price and no control over the host
— acceptable for a stateless HTTP API.

Fargate rather than Lambda for the API: an always-warm process with a live
database connection pool suits a request/response API with a relational backend.
Lambda would pay a cold start and open a connection per invocation, and
connection exhaustion against RDS would arrive long before any scaling benefit
did. Lambda *is* used where it fits — the optional export consumer, which is
bursty and short-lived.

### Two services, one image

`bridgecore-api` and `bridgecore-worker` run the same image with different
entrypoints (`/app/bridgecore-api`, `/app/bridgecore-worker`). One artifact to
build, scan and promote, so the worker can never be running different business
logic than the API.

They are separate services because export throughput should scale independently
of API traffic: a tenant generating a 500,000-row export should not compete for
CPU with request handling, and the worker count should be changeable without
redeploying the API.

The worker has `deployment_minimum_healthy_percent = 0` — it serves no traffic, so
replacing it outright is fine. A job claimed by a task that dies is reclaimed by
another after the visibility timeout.

### Autoscaling

Two target-tracking policies on the API service:

- **CPU at 65%.**
- **ALB request count per target at 800.** This reacts first for an I/O-bound API,
  where tasks queue on the database long before they saturate a core.

Scale-out cooldown 60s, scale-in 300s: a premature scale-in during a spike causes
the exact latency the scaling exists to prevent.

Minimum 2 tasks, so a rolling deploy or an AZ loss never leaves zero.

### The container image

Multi-stage build, `CGO_ENABLED=0` for a static binary, then
`gcr.io/distroless/static-debian12:nonroot`.

Distroless means no shell, no package manager, no busybox. If the API is
compromised there is nothing in the image to pivot with — an attacker cannot spawn
`/bin/sh` or fetch a payload, because neither exists. Runs as UID 65532.

Two real costs, both accepted:

1. `docker exec` debugging is impossible.
2. **ECS container health checks cannot be used**, because every form of them
   requires a shell to run the command. Health is therefore judged by the ALB
   target group polling `/ready` — which is the signal that actually gates traffic
   anyway.

CI asserts that no shell exists in the built image, so this cannot silently
regress when someone swaps the base image for Alpine to make debugging easier.

---

## 3. Load balancing

The target group health-checks **`/ready`**, not `/health` and not `/live`. The
three are genuinely different and conflating them is a common way to build an
unrecoverable deployment:

- `/live` — is the process running? Never checks a dependency. If a *liveness*
  probe failed when the database was briefly unreachable, the orchestrator would
  kill every task at once and turn a recoverable blip into a total outage.
- `/ready` — should this task receive traffic? Checks Postgres and Redis, so a
  task with a broken dependency is quietly removed from the load balancer and put
  back when it recovers.
- `/health` — the human view: versions, uptime, queue depth, dependency detail.

The ALB idle timeout is 65s, set *above* the application's 60s write timeout, so
the application decides when a response has taken too long rather than the load
balancer severing it. Exports are asynchronous precisely so no request ever
approaches this.

`deregistration_delay = 30` lets in-flight requests finish during a deploy instead
of becoming 502s. `drop_invalid_header_fields = true` rejects malformed headers at
the edge.

TLS policy `ELBSecurityPolicy-TLS13-1-2-2021-06`: the policies still permitting
TLS 1.0/1.1 exist for legacy browsers, and a server-consumed API has none. With no
certificate configured, only an HTTP listener is created — documented as demo-only,
since bearer tokens over plaintext are readable in transit.

---

## 4. Data stores

### RDS PostgreSQL 16

- Private subnets, encrypted with gp3 storage, storage autoscaling to a ceiling
  (so the database does not hit a wall at 3am).
- **`manage_master_user_password = true`** — RDS creates and rotates its own
  credential in Secrets Manager, so Terraform never handles the password and it
  never appears in state.
- **`rds.force_ssl = 1`** in the parameter group. The application setting
  `DB_SSLMODE=require` is a client-side control; forcing it server-side means a
  misconfigured client fails to connect rather than silently connecting in
  plaintext.
- **`log_min_duration_statement = 1000`** — logs any statement over a second. This
  is how the N+1 queries the DataLoader exists to prevent get caught in production
  rather than guessed at.
- `skip_final_snapshot = false` and deletion protection on: an accidental
  `terraform destroy` costs a restore, not the data.
- `ignore_changes = [engine_version]`, so an automatic minor upgrade does not make
  the next apply want to downgrade the engine.

The security group has **no egress rules** — the database has no legitimate reason
to originate a connection, and denying it removes an exfiltration path.

Multi-AZ is off by default (`db_multi_az`). It roughly doubles cost and is the
difference between a failover and an outage; that is a deliberate choice to
surface rather than to bury.

### ElastiCache Redis

Holds rate-limit counters and nothing durable. Consequences of that:

- `maxmemory-policy = volatile-lru`. Evicting old keys under pressure is strictly
  better than refusing writes, because a failed rate-limit write would reject a
  legitimate request.
- `snapshot_retention_limit = 0` — nothing here is worth restoring.
- Encryption in transit is enabled, which is why the application exposes
  `REDIS_TLS`: the client must be told to speak TLS or every command fails with a
  protocol error.

---

## 5. Export storage and queueing

### S3

One private bucket. Access is exclusively via presigned URLs minted by the API
after it has verified the requester owns the job.

- All four public-access blocks, unconditionally. A "temporarily public" export
  bucket is how cross-tenant leaks become news stories.
- Encryption at rest with bucket keys; a bucket policy that **denies non-TLS
  requests**, because a presigned URL is a capability and one fetched over
  plaintext HTTP is readable in transit.
- Versioning **suspended**: exports are reproducible from the usage table, so
  versioning would only multiply storage of derived data.
- Lifecycle rules expire export objects after `export_object_expiration_days` and
  abort incomplete multipart uploads after 3 days (a worker killed mid-upload
  otherwise leaves storage consuming bytes that are invisible in the object
  listing).

Expiring exports bounds both the bill and the blast radius of a future credential
leak: there is simply less historical tenant data in the bucket to steal.

### SQS

**The queue is a notification channel, not the system of record.** Job state lives
in the `export_jobs` table, claimed with `FOR UPDATE SKIP LOCKED`. SQS exists so a
consumer starts work immediately instead of waiting for the worker's next poll.

If the queue is unavailable or a message is lost, the job is still in the table
and still gets processed — the export is late, not lost. Making the queue
authoritative would trade a `SKIP LOCKED` query for a distributed consistency
problem and job state that cannot be inspected with SQL.

- Visibility timeout 900s, which must exceed the consumer's worst-case runtime or
  a slow export is redelivered while it is still being generated.
- Long polling (`receive_wait_time_seconds = 20`): cheaper and lower latency than
  spinning on empty receives.
- A DLQ with `maxReceiveCount = 3` and 14-day retention. After three failures a
  message is quarantined rather than retried forever — a single poison message
  otherwise consumes an entire worker fleet. Long retention because the whole
  point is to still have it when someone investigates next week.

### The S3 gateway endpoint

Export uploads go over a VPC gateway endpoint rather than through NAT. That keeps
them inside the AWS network and removes them from the NAT data-processing bill,
which for a bulk-upload workload is the line that actually shows up on the
invoice.

---

## 6. Secrets

The task definition contains **only ARNs**. The ECS agent resolves them into the
container environment before the process starts, using the execution role.

```hcl
secrets = [
  { name = "JWT_ACCESS_SECRET", valueFrom = "${aws_secretsmanager_secret.app.arn}:JWT_ACCESS_SECRET::" },
  { name = "DB_PASSWORD",       valueFrom = "${data.aws_secretsmanager_secret.rds.arn}:password::" },
]
```

So: a leaked task definition leaks nothing, a leaked image leaks nothing, and
rotating a secret needs no rebuild or re-registration.

Terraform generates the JWT secrets, operator token and export signing key once,
then `ignore_changes = [secret_string]`. Without that, every apply would rotate
the signing keys and log every user out.

The application also supports loading the bundle itself
(`AWS_SECRETS_MANAGER_SECRET_ID`), which is how the Lambda gets its configuration
and how a local process can be pointed at real secrets for debugging.

---

## 7. IAM

**Two roles per task**, because they are used at different times by different
principals:

| Role | Used by | Permitted |
| --- | --- | --- |
| Execution | ECS agent, before the container starts | Pull from ECR, write logs, read exactly two secrets |
| Task | The application, at runtime | `PutObject`/`GetObject` under one S3 prefix, one SQS queue, read its own secret |

Collapsing them would hand the application the ability to pull arbitrary images
and read every secret the agent can — permissions it has no use for.

The task role deliberately **lacks `s3:ListBucket`**. It can touch keys it already
knows, but a compromised task cannot enumerate which tenants have exports.

### The GitHub OIDC deploy role

No long-lived AWS keys exist in GitHub. The workflow presents a short-lived OIDC
token; AWS validates it against the trust policy and returns credentials that
expire within the hour.

```hcl
condition {
  test     = "StringLike"
  variable = "token.actions.githubusercontent.com:sub"
  values = [
    "repo:${var.github_repository}:ref:refs/heads/main",
    "repo:${var.github_repository}:environment:production",
  ]
}
```

**Without that condition, any GitHub repository in the world could assume the
role.** It is the single most important line in `iam.tf`.

Two further constraints:

- `ecs:UpdateService` carries an `ecs:cluster` condition, so a compromised
  workflow cannot redeploy unrelated services in the account.
- `iam:PassRole` is scoped to exactly the two task roles, with an
  `iam:PassedToService` condition. With a wildcard it is account takeover: the
  pipeline could launch a task as any role, including admin.

---

## 8. The optional export Lambda

`cmd/lambda-export` is an alternative consumer of the same queue and the same job
table. Which one you run is operational, not architectural:

| | Worker service | Lambda |
| --- | --- | --- |
| Best for | Steady export volume | Bursty or infrequent |
| Cost | Always running | Per export |
| Cold start | None | Yes |
| DB connections | Warm pool | One per cold start |

Both are safe to run simultaneously, because claiming uses `FOR UPDATE SKIP
LOCKED`: whichever consumer claims a job owns it, and the other finds nothing.

Details worth noting:

- `provided.al2023` with a binary named `bootstrap` — Go has no managed runtime.
  The custom runtime protocol is implemented directly over `net/http` (three HTTP
  calls), consistent with this project's no-SDK approach.
- `reserved_concurrent_executions = 2`. An unbounded Lambda fan-out is the fastest
  way to exhaust the RDS connection limit and take the API down with it.
- `batch_size = 1`: batching would mean one failure re-delivers every job in the
  batch, and exports are long enough that partial-batch-failure bookkeeping is not
  worth the complexity.
- Timeout 600s, comfortably under the queue's 900s visibility timeout, so a
  message is never redelivered while still being processed.
- The database handle is created during the cold start and reused across warm
  invocations.

The Lambda is created only when `enable_export_lambda = true` **and** the
deployment package exists, so `terraform plan` on a fresh clone does not fail on a
missing zip.

---

## 9. Observability

Log groups per service with configurable retention (logs kept forever are a
growing bill and a growing liability). Container Insights on the cluster.

Alarms publish to an SNS topic. Each is meant to be actionable — an alarm that
fires routinely and is ignored is worse than no alarm, because it trains people to
ignore the others.

The one worth highlighting is built from the application's own structured logs:

```hcl
resource "aws_cloudwatch_log_metric_filter" "cross_tenant_denied" {
  pattern = "{ $.event = \"security.cross_tenant_denied\" }"
  ...
}
```

Cross-tenant access returns 404 by design, so the attacker learns nothing — and
neither would an operator, without this. The metric filter turns those denials
into an alarmable signal: isolation held, but someone is enumerating, and the
audit log says which tenant and which IDs.

Others: 5xx count, p99 latency (not average — an average hides the tail users
notice), unhealthy host count, RDS CPU and free storage, export queue depth, and
**any** message reaching the DLQ (threshold zero: a job exhausting its retry
budget should never happen quietly).

A dashboard ties them together; `terraform output dashboard_url` opens it.

---

## 10. What is deliberately not here

- **A WAF.** Would add managed rules for common injection and bot patterns in
  front of the ALB. Worth adding for a real deployment.
- **A read replica or RDS Proxy.** Read and write load share one instance; a heavy
  export competes with request traffic for I/O.
- **Route 53 and ACM records.** The ALB DNS name and zone ID are outputs so you
  can point a record at them, but DNS is left to the operator.
- **Automatic secret rotation schedules.** Supported by Secrets Manager,
  unconfigured here.
- **Cross-region anything.** Single region, single deployment.
- **Registry Terraform modules.** Everything is written out explicitly. For a
  portfolio codebase the resources and their relationships *are* the content;
  `terraform-aws-modules/vpc` would hide exactly the part worth reading.
