# Deployment

How BridgeCore gets to AWS, how a deploy behaves when it goes wrong, and the
operational tasks that come up afterwards.

---

## 1. First-time setup

### Prerequisites

- Terraform 1.6+, AWS CLI v2, credentials with permission to create VPC, ECS,
  RDS, ElastiCache, S3, SQS, IAM, Secrets Manager and CloudWatch resources.
- A GitHub repository for the code.
- Optional but strongly recommended: an ACM certificate. Without one only an
  HTTP listener is created, and bearer tokens over plaintext HTTP are readable by
  anything on the network path — fine for a demo, never for real traffic.

### Step 1 — Provision infrastructure

```bash
cd infra/terraform
terraform init

terraform plan \
  -var 'github_repository=your-org/bridgecore' \
  -var 'alarm_email=you@example.com' \
  -var 'certificate_arn=arn:aws:acm:ap-south-1:123456789012:certificate/…'
```

Read the plan. It creates roughly 70 resources; the ones worth checking are the
RDS instance class, the NAT gateway count (`single_nat_gateway` defaults to true
— cheaper, but the single NAT is a single point of failure for outbound traffic),
and the IAM trust policy on the deploy role.

```bash
terraform apply -var 'github_repository=your-org/bridgecore' -var 'alarm_email=you@example.com'
```

RDS takes 5–10 minutes. The ECS services will fail to stabilise on this first
apply because no image exists in ECR yet — that is expected, and the first
pipeline run resolves it.

> **Enable remote state before anyone else touches this.** The S3 backend in
> `versions.tf` is commented out so `init -backend=false` works in CI and a
> first-time reader can plan without provisioning a bucket. Local state cannot be
> locked, so two people — or a person and a pipeline — applying concurrently will
> corrupt it.

### Step 2 — Collect the outputs

```bash
terraform output                              # everything non-sensitive
terraform output -raw github_deploy_role_arn  # → GitHub secret AWS_DEPLOY_ROLE_ARN
terraform output -raw api_url                 # → GitHub secret PRODUCTION_BASE_URL
terraform output -raw platform_admin_token    # keep somewhere safe; not recoverable from the console
```

The operator token is the credential for `/api/v1/platform/*`. Treat it like a
root credential: it crosses every tenant boundary.

### Step 3 — Configure GitHub

Repository secrets:

| Secret | Value |
| --- | --- |
| `AWS_DEPLOY_ROLE_ARN` | `terraform output -raw github_deploy_role_arn` |
| `PRODUCTION_BASE_URL` | `terraform output -raw api_url` |

Then create a **`production` environment** in repository settings. The deploy job
targets it, so adding required reviewers there gives you a manual approval gate
without touching the workflow. The OIDC trust policy already permits both
`ref:refs/heads/main` and `environment:production`.

Update the `env:` block at the top of `.github/workflows/deploy.yml` if you
changed `aws_region`, `project` or `environment` from the defaults — the cluster
and service names there must match the Terraform outputs.

### Step 4 — First deploy

```bash
git push origin main
```

The pipeline builds, pushes an image tagged with the commit SHA, registers a task
definition, updates the service, waits for stability, and smoke-tests `/health`.

If the ECS service was created before any image existed, the first update is what
brings it up. Watch it:

```bash
aws ecs describe-services --cluster bridgecore-production-cluster \
  --services bridgecore-api --query 'services[0].deployments'
```

### Step 5 — Seed (optional)

There is no automatic production seed, deliberately: demo tenants with known
passwords should not appear in a real environment. To seed a staging environment,
run the seed binary as a one-off task:

```bash
aws ecs run-task \
  --cluster bridgecore-production-cluster \
  --task-definition bridgecore-production-api \
  --launch-type FARGATE \
  --network-configuration 'awsvpcConfiguration={subnets=[subnet-…],securityGroups=[sg-…],assignPublicIp=DISABLED}' \
  --overrides '{"containerOverrides":[{"name":"bridgecore-api","command":["/app/bridgecore-seed"]}]}'
```

---

## 2. What a normal deploy does

```
push to main
   │
   ├─ 1. go vet + go test -race                  last cheap place to stop a bad build
   ├─ 2. assume AWS role via OIDC                short-lived credentials, no stored keys
   ├─ 3. docker build + push  ⟶  ECR:<sha>       immutable tag
   │
   ├─ 4. capture the CURRENT task definition     the rollback target, recorded before the change
   ├─ 5. render a new task definition (new image)
   ├─ 6. update the service, wait for stability  up to 10 minutes
   ├─ 7. redeploy the worker (non-blocking)
   ├─ 8. smoke test /health, verify the SHA
   └─ on any failure: revert to the definition from step 4 and wait for stable
```

Rolling update parameters: `deployment_minimum_healthy_percent = 100` and
`maximum_percent = 200` — start the new tasks, then retire the old ones, so
capacity never dips. Tasks are only sent traffic once the ALB target group reports
`/ready` healthy, and the target group has a 30-second deregistration delay so
in-flight requests finish rather than becoming 502s.

### Two independent rollback mechanisms

1. **The ECS deployment circuit breaker** (`deployment_circuit_breaker { enable =
   true, rollback = true }`). If the new task set never becomes healthy, ECS
   reverts by itself — before the pipeline's own rollback step is even reached.
   This is what makes automated deployment defensible: the safety net does not
   depend on the pipeline still running.
2. **The pipeline's explicit rollback**, which handles the case the circuit
   breaker cannot see: tasks that *are* healthy but wrong — a bad migration, a
   config error that passes `/ready` but fails `/health`, a smoke test that
   catches a broken endpoint.

### Migrations

The API runs migrations on boot; the worker explicitly does not. Exactly one
component owns schema changes, because two migrators racing during a rolling
deploy is how a half-applied migration happens.

The practical consequence: **migrations must be backward compatible with the
previous release**, because during a rolling deploy old and new tasks run
simultaneously against the migrated schema. In order:

- Adding a nullable column, a table, or an index — safe.
- Dropping or renaming a column — two releases. Release 1 stops using it;
  release 2 drops it.
- Adding a `NOT NULL` column — three steps: add nullable, backfill, then add the
  constraint.
- `CREATE INDEX` on a large table — use `CONCURRENTLY`, outside a transaction, or
  it takes a write lock for the duration.

After editing anything in `/migrations`, run `make sync-migrations` to update the
embedded copy the binary ships with. CI does not currently catch a divergence
here; it is worth adding.

---

## 3. Manual rollback

If a deploy landed and you need to go back:

```bash
CLUSTER=bridgecore-production-cluster

# List recent task definition revisions.
aws ecs list-task-definitions --family-prefix bridgecore-production-api --sort DESC --max-items 10

# Revert.
aws ecs update-service --cluster $CLUSTER --service bridgecore-api \
  --task-definition bridgecore-production-api:41 --force-new-deployment

aws ecs wait services-stable --cluster $CLUSTER --services bridgecore-api
```

Or re-run the deploy workflow with `workflow_dispatch` and an earlier image tag.

**A rollback does not undo a migration.** If the bad release migrated the schema,
reverting the image leaves the old code against the new schema. This is exactly
why the backward-compatibility rules above are not optional — they are what makes
rollback a safe operation rather than a second outage.

---

## 4. Scaling

**API tasks** autoscale on two signals: average CPU (target 65%) and ALB request
count per target (target 800). Request-rate scaling reacts first for an I/O-bound
API, where tasks queue on the database long before they saturate a core. Scale-out
cooldown is 60s and scale-in 300s — a premature scale-in during a spike causes the
exact latency the scaling exists to prevent.

Adjust bounds with `api_min_capacity` / `api_max_capacity`. Keep the minimum at 2
or more so a rolling deploy or an AZ loss never leaves zero.

**Export workers** scale by changing `worker_desired_count`. No coordination is
needed: claiming uses `FOR UPDATE SKIP LOCKED`, so workers never duplicate work
and no leader election is involved.

**Database.** `db_instance_class` for vertical scaling; `db_multi_az = true` for
failover (roughly doubles cost, and is the difference between a failover and an
outage). Storage autoscales up to `db_max_allocated_storage`.

Before scaling tasks out, check the connection budget: `DB_MAX_OPEN_CONNS` ×
(API tasks + worker tasks) must stay comfortably below the instance's
`max_connections`, or a scale-out event causes new tasks to fail to connect. At
the defaults (25 × 8 = 200) a `db.t4g.micro` is already the binding constraint —
either raise the instance class or lower the pool before scaling past ~6 tasks.

---

## 5. Monitoring

`terraform output dashboard_url` opens the CloudWatch dashboard: request and error
rates, p50/p99 latency, ECS utilisation, export queue depth, database CPU and
connections, and cross-tenant denial counts.

Alarms publish to the SNS topic (`alarm_topic_arn`); subscribe Slack or PagerDuty
there. Each one is meant to be actionable — an alarm that fires routinely and is
ignored is worse than no alarm, because it trains people to ignore the others.

| Alarm | Means | First thing to check |
| --- | --- | --- |
| `api-5xx` | Server errors | The API log group, filtered by the correlated request IDs |
| `api-latency` | p99 above target | RDS Performance Insights for slow queries |
| `api-unhealthy-hosts` | Tasks failing `/ready` | `GET /health` on a task; usually the database or Redis |
| `rds-cpu` | Sustained high CPU | An unindexed query, or an export scanning more than it should |
| `rds-free-storage` | Storage low | Whether autoscaling is keeping up |
| `export-queue-depth` | Exports backing up | Worker task count; the worker log group |
| `export-dlq` | A job exhausted its retries | The DLQ message and the `export_jobs` row |
| `cross-tenant-attempts` | Someone is enumerating | The audit log: which tenant, which IDs |

### Finding a specific request

Every response carries `X-Request-ID`, and it appears in every log line for that
request:

```
fields @timestamp, tenant_id, route, status_code, latency_ms, @message
| filter request_id = "8f14e45f-ea2b-4c1f-9b3a-1d4c8e77a0b2"
| sort @timestamp asc
```

Support tickets should start with that ID rather than a timestamp guess.

### Slowest endpoints by tenant

```
fields @timestamp, tenant_id, route, latency_ms
| filter ispresent(latency_ms)
| stats avg(latency_ms) as avg, pct(latency_ms, 99) as p99, count(*) as n by tenant_id, route
| sort p99 desc
| limit 25
```

---

## 6. Operational tasks

### Provision a tenant

```bash
curl -sX POST "$API_URL/api/v1/platform/tenants" \
  -H "X-Platform-Token: $PLATFORM_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme Corp","slug":"acme","plan":"pro"}'
```

### Change a plan

```bash
curl -sX PUT "$API_URL/api/v1/platform/tenants/$TENANT_ID" \
  -H "X-Platform-Token: $PLATFORM_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"plan":"enterprise"}'
```

Takes effect on the tenant's next request, since auth re-reads the tenant row.

### Grant a feature outside the plan

```bash
curl -sX POST "$API_URL/api/v1/platform/features/grant" \
  -H "X-Platform-Token: $PLATFORM_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"…","feature_key":"usage.export","enabled":true}'
```

Audited as `feature.granted`. This should be the only source of that event; one
attributed to anything else is worth investigating.

### Suspend a tenant

```bash
curl -sX PUT "$API_URL/api/v1/platform/tenants/$TENANT_ID" \
  -H "X-Platform-Token: $PLATFORM_TOKEN" \
  -H 'Content-Type: application/json' -d '{"is_active":false}'
```

Effective immediately — existing tokens stop working on their next request rather
than at expiry.

### Rotate a secret

Update the value in Secrets Manager, then force a new deployment so tasks pick it
up:

```bash
aws secretsmanager put-secret-value --secret-id bridgecore-production/app \
  --secret-string '{"JWT_ACCESS_SECRET":"…","JWT_REFRESH_SECRET":"…","PLATFORM_ADMIN_TOKEN":"…","EXPORT_SIGNING_KEY":"…"}'

aws ecs update-service --cluster bridgecore-production-cluster --service bridgecore-api --force-new-deployment
```

Rotating a JWT secret invalidates every issued token: users must log in again.
Rotating `EXPORT_SIGNING_KEY` invalidates outstanding local download URLs. Do not
change these in Terraform — the secret version has `ignore_changes` precisely so
an unrelated apply cannot log everyone out.

### Redrive dead-lettered exports

```bash
aws sqs start-message-move-task \
  --source-arn "$(terraform output -raw export_dlq_url | sed 's|https://sqs\.\([^.]*\)\.amazonaws\.com/\([0-9]*\)/\(.*\)|arn:aws:sqs:\1:\2:\3|')"
```

Investigate the `export_jobs` row first. The queue message is only a pointer;
redriving it without fixing the underlying failure just repeats it three more
times.

### Get a database shell

RDS is private, so go through a task:

```bash
# Only works in non-production, where enable_execute_command is true.
aws ecs execute-command --cluster bridgecore-production-cluster \
  --task <task-id> --container bridgecore-api --interactive --command "/bin/sh"
```

This will fail in production, and on the distroless image there is no shell
anyway — that is the intended posture. For production access, use a bastion or an
SSM session to a small instance in a private subnet.

---

## 7. Local and staging

### Local

```bash
make up       # postgres, redis, api, worker, then seed
make logs
make down     # stop, keep data
make reset    # stop and wipe volumes
```

The Compose stack deliberately mirrors production topology: the API runs with
`EXPORT_IN_PROCESS_WORKER=false` and a separate worker service consumes the queue,
so a bug that only appears when the worker is a different process appears locally
too.

### Staging

Re-run Terraform in a separate workspace or state file with
`-var 'environment=staging'`. Sensible staging overrides: `db_multi_az=false`,
`api_min_capacity=1`, `db_deletion_protection=false`, `log_retention_days=7`.

Note that `environment=staging` also enables `enable_execute_command`, which is
intentional — that is where you want to be able to get into a task.

---

## 8. Cost

Rough monthly figures at the defaults (ap-south-1, light traffic):

| Resource | Approx. |
| --- | --- |
| ECS Fargate, 2 API tasks (0.5 vCPU / 1 GB) | $25 |
| ECS Fargate, 1 worker task (0.25 vCPU / 0.5 GB) | $7 |
| ALB | $18 + traffic |
| RDS `db.t4g.micro`, 20 GB gp3, single-AZ | $15 |
| ElastiCache `cache.t4g.micro` | $12 |
| NAT gateway (single) | $32 + data |
| S3, SQS, Secrets Manager, CloudWatch | $5–10 |
| **Total** | **~$115–125** |

The NAT gateway is often the surprise. `single_nat_gateway = true` is the default
for that reason; the S3 gateway endpoint exists so bulk export uploads bypass NAT
data processing entirely, which is the difference that shows up on the invoice for
an export-heavy workload.

`db_multi_az = true` adds roughly $15 and per-AZ NAT adds roughly $32 per extra
AZ. Both are availability purchases, not performance ones.

### Tearing it down

```bash
cd infra/terraform
terraform destroy
```

This will fail while `db_deletion_protection` and the ALB's deletion protection
are enabled — that is the point. Set
`-var 'db_deletion_protection=false' -var 'environment=staging'` to allow it. RDS
still takes a final snapshot (`skip_final_snapshot = false`), so an accidental
destroy costs a restore rather than the data. Delete the snapshot manually
afterwards if you truly want it gone.
