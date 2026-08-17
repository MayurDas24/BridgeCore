# BridgeCore production infrastructure.
#
# Layout, and why each file exists:
#
#   network.tf         VPC, public/private subnets, NAT, routing
#   security_groups.tf the actual isolation boundary between tiers
#   ecr.tf             image registry with scanning and lifecycle
#   alb.tf             public entry point, TLS termination, health checks
#   ecs.tf             Fargate cluster, API and worker services, autoscaling
#   rds.tf             PostgreSQL, private, encrypted, backed up
#   redis.tf           ElastiCache, private, encrypted
#   s3.tf              private export bucket with lifecycle rules
#   sqs.tf             export queue plus dead-letter queue
#   lambda.tf          optional serverless export consumer
#   secrets.tf         Secrets Manager entries and generated secrets
#   iam.tf             task roles, execution role, GitHub OIDC deploy role
#   cloudwatch.tf      log groups, alarms, dashboard
#   outputs.tf         the values you need after an apply
#
# Nothing here is a module from the registry. That is deliberate for a
# portfolio codebase: the point is to show the resources and their
# relationships, not to show that terraform-aws-modules exists.

locals {
  name = "${var.project}-${var.environment}"

  common_tags = {
    Project     = var.project
    Environment = var.environment
    ManagedBy   = "terraform"
    Repository  = "bridgecore"
  }
}

data "aws_availability_zones" "available" {
  state = "available"
}

data "aws_caller_identity" "current" {}

data "aws_region" "current" {}

locals {
  export_prefix = "usage-exports"

  # The Lambda is only created when it is enabled AND its package has been
  # built, so a plan on a fresh clone does not fail on a missing zip file.
  enable_export_lambda = var.enable_export_lambda && fileexists(var.export_lambda_zip_path)
}
