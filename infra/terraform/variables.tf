variable "aws_region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "ap-south-1"
}

variable "project" {
  description = "Project name, used as the prefix for every resource name."
  type        = string
  default     = "bridgecore"
}

variable "environment" {
  description = "Deployment environment (production, staging)."
  type        = string
  default     = "production"

  validation {
    condition     = contains(["production", "staging", "development"], var.environment)
    error_message = "environment must be one of production, staging, development."
  }
}

# ---- Networking ----

variable "vpc_cidr" {
  description = "CIDR block for the VPC."
  type        = string
  default     = "10.20.0.0/16"
}

variable "availability_zone_count" {
  description = "Number of AZs to spread subnets across. Two is the minimum for a highly available ALB and a Multi-AZ RDS."
  type        = number
  default     = 2

  validation {
    condition     = var.availability_zone_count >= 2
    error_message = "At least two availability zones are required: an ALB needs subnets in two AZs, and a single-AZ deployment has no failover target."
  }
}

variable "single_nat_gateway" {
  description = "Use one NAT gateway instead of one per AZ. Cheaper, but the single NAT is an availability single point of failure for outbound traffic."
  type        = bool
  default     = true
}

# ---- Application ----

variable "app_port" {
  description = "Container port the API listens on."
  type        = number
  default     = 8080
}

variable "api_desired_count" {
  description = "Number of API tasks to run. Two or more so a rolling deploy or an AZ loss never leaves zero."
  type        = number
  default     = 2
}

variable "api_cpu" {
  description = "Fargate CPU units for the API task (1024 = 1 vCPU)."
  type        = number
  default     = 512
}

variable "api_memory" {
  description = "Fargate memory (MiB) for the API task."
  type        = number
  default     = 1024
}

variable "worker_desired_count" {
  description = "Number of export worker tasks. Safe to scale: job claiming uses FOR UPDATE SKIP LOCKED, so workers never duplicate work."
  type        = number
  default     = 1
}

variable "worker_cpu" {
  description = "Fargate CPU units for the worker task."
  type        = number
  default     = 256
}

variable "worker_memory" {
  description = "Fargate memory (MiB) for the worker task."
  type        = number
  default     = 512
}

variable "api_min_capacity" {
  description = "Minimum API task count for autoscaling."
  type        = number
  default     = 2
}

variable "api_max_capacity" {
  description = "Maximum API task count for autoscaling."
  type        = number
  default     = 6
}

variable "image_tag" {
  description = "Container image tag to deploy. CD overrides this per commit; 'latest' is only a bootstrap default."
  type        = string
  default     = "latest"
}

# ---- Database ----

variable "db_instance_class" {
  description = "RDS instance class."
  type        = string
  default     = "db.t4g.micro"
}

variable "db_allocated_storage" {
  description = "Initial RDS storage in GiB."
  type        = number
  default     = 20
}

variable "db_max_allocated_storage" {
  description = "Storage autoscaling ceiling in GiB. Set above allocated_storage to enable it, so the database does not hit a wall at 3am."
  type        = number
  default     = 100
}

variable "db_name" {
  description = "Initial database name."
  type        = string
  default     = "bridgecore"
}

variable "db_username" {
  description = "Master database username."
  type        = string
  default     = "bridgecore"
}

variable "db_multi_az" {
  description = "Run RDS Multi-AZ. Roughly doubles cost and is the difference between a failover and an outage."
  type        = bool
  default     = false
}

variable "db_backup_retention_days" {
  description = "Automated backup retention in days. Zero disables backups entirely, which is never appropriate for production."
  type        = number
  default     = 7

  validation {
    condition     = var.db_backup_retention_days >= 1
    error_message = "Backup retention must be at least 1 day."
  }
}

variable "db_deletion_protection" {
  description = "Refuse to destroy the database. Leave on for anything holding real data."
  type        = bool
  default     = true
}

# ---- Cache ----

variable "redis_node_type" {
  description = "ElastiCache node type."
  type        = string
  default     = "cache.t4g.micro"
}

variable "redis_engine_version" {
  description = "ElastiCache Redis engine version."
  type        = string
  default     = "7.1"
}

# ---- Observability & alarms ----

variable "log_retention_days" {
  description = "CloudWatch log retention. Logs kept forever are a growing bill and a growing liability."
  type        = number
  default     = 30
}

variable "alarm_email" {
  description = "Email address subscribed to the alarm topic. Leave empty to create the topic without a subscription."
  type        = string
  default     = ""
}

variable "latency_alarm_threshold_seconds" {
  description = "p99 target response time that triggers the latency alarm."
  type        = number
  default     = 1.5
}

variable "http_5xx_alarm_threshold" {
  description = "Number of 5xx responses in a period before alarming."
  type        = number
  default     = 10
}

variable "export_queue_depth_alarm_threshold" {
  description = "SQS queue depth before alarming that exports are backing up."
  type        = number
  default     = 100
}

# ---- Security ----

variable "cors_allowed_origins" {
  description = "Comma-separated CORS allow-list passed to the application. The app refuses to start in production with a wildcard."
  type        = string
  default     = "https://app.example.com"
}

variable "alb_ingress_cidrs" {
  description = "CIDRs permitted to reach the load balancer. Narrow this for an internal-only deployment."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "certificate_arn" {
  description = "ACM certificate ARN for HTTPS. When empty, only an HTTP listener is created (acceptable for a demo, never for production traffic carrying tokens)."
  type        = string
  default     = ""
}

variable "export_url_ttl" {
  description = "How long a presigned export download URL stays valid."
  type        = string
  default     = "15m"
}

variable "export_object_expiration_days" {
  description = "Days after which generated export objects are deleted by an S3 lifecycle rule. Exports are reproducible, so retaining them indefinitely only grows cost and blast radius."
  type        = number
  default     = 30
}

# ---- CI/CD ----

variable "github_repository" {
  description = "GitHub repository allowed to assume the deploy role, as owner/name. Leave empty to skip creating the OIDC deploy role."
  type        = string
  default     = ""
}

variable "create_github_oidc_provider" {
  description = "Create the GitHub OIDC provider. Set false if one already exists in the account (only one per account is permitted)."
  type        = bool
  default     = true
}

variable "enable_export_lambda" {
  description = "Deploy the serverless export consumer alongside the worker service. The Lambda and the worker are alternative consumers of the same queue and job table."
  type        = bool
  default     = false
}

variable "export_lambda_zip_path" {
  description = "Path to the built Lambda deployment package. Build it with `make lambda-build`."
  type        = string
  default     = "../../bin/lambda-export.zip"
}
