output "api_url" {
  description = "Base URL of the deployed API."
  value       = var.certificate_arn == "" ? "http://${aws_lb.main.dns_name}" : "https://${aws_lb.main.dns_name}"
}

output "alb_dns_name" {
  description = "Load balancer DNS name — point your CNAME here."
  value       = aws_lb.main.dns_name
}

output "alb_zone_id" {
  description = "Load balancer hosted zone ID, for a Route 53 alias record."
  value       = aws_lb.main.zone_id
}

output "ecr_repository_url" {
  description = "ECR repository the deploy pipeline pushes to."
  value       = aws_ecr_repository.app.repository_url
}

output "ecs_cluster_name" {
  description = "ECS cluster name (set as ECS_CLUSTER in the deploy workflow)."
  value       = aws_ecs_cluster.main.name
}

output "ecs_api_service_name" {
  description = "API service name (set as ECS_SERVICE in the deploy workflow)."
  value       = aws_ecs_service.api.name
}

output "ecs_worker_service_name" {
  description = "Worker service name (set as ECS_WORKER_SERVICE in the deploy workflow)."
  value       = aws_ecs_service.worker.name
}

output "github_deploy_role_arn" {
  description = "Role ARN for the GitHub Actions OIDC deploy. Store as the AWS_DEPLOY_ROLE_ARN repository secret."
  value       = var.github_repository == "" ? "" : aws_iam_role.github_deploy[0].arn
}

output "database_endpoint" {
  description = "RDS endpoint. Private — reachable only from within the VPC."
  value       = aws_db_instance.main.address
}

output "database_secret_arn" {
  description = "ARN of the RDS-managed master credential secret."
  value       = data.aws_secretsmanager_secret.rds.arn
}

output "redis_endpoint" {
  description = "ElastiCache primary endpoint. Private, TLS required."
  value       = aws_elasticache_replication_group.main.primary_endpoint_address
}

output "app_secret_arn" {
  description = "ARN of the application secret bundle. Rotate values here, not in Terraform."
  value       = aws_secretsmanager_secret.app.arn
}

output "exports_bucket" {
  description = "Private S3 bucket holding generated usage exports."
  value       = aws_s3_bucket.exports.bucket
}

output "export_queue_url" {
  description = "SQS queue URL for export notifications."
  value       = aws_sqs_queue.exports.url
}

output "export_dlq_url" {
  description = "Dead-letter queue holding exports that exhausted their retries."
  value       = aws_sqs_queue.export_dlq.url
}

output "dashboard_url" {
  description = "CloudWatch dashboard for the deployment."
  value       = "https://${var.aws_region}.console.aws.amazon.com/cloudwatch/home?region=${var.aws_region}#dashboards:name=${aws_cloudwatch_dashboard.main.dashboard_name}"
}

output "alarm_topic_arn" {
  description = "SNS topic that alarms publish to. Subscribe additional endpoints here."
  value       = aws_sns_topic.alarms.arn
}

# The generated platform operator token. Needed to call /api/v1/platform/*,
# and marked sensitive so it is not printed by `terraform apply`. Read it with:
#   terraform output -raw platform_admin_token
output "platform_admin_token" {
  description = "Operator token for the cross-tenant control plane."
  value       = random_password.platform_admin_token.result
  sensitive   = true
}
