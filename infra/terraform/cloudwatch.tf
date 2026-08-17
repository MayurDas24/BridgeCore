resource "aws_cloudwatch_log_group" "api" {
  name              = "/ecs/${local.name}/api"
  retention_in_days = var.log_retention_days

  tags = { Name = "${local.name}-api-logs" }
}

resource "aws_cloudwatch_log_group" "worker" {
  name              = "/ecs/${local.name}/worker"
  retention_in_days = var.log_retention_days

  tags = { Name = "${local.name}-worker-logs" }
}

resource "aws_cloudwatch_log_group" "export_lambda" {
  count = local.enable_export_lambda ? 1 : 0

  name              = "/aws/lambda/${local.name}-usage-export"
  retention_in_days = var.log_retention_days

  tags = { Name = "${local.name}-export-lambda-logs" }
}

# ---- Alerting ----

resource "aws_sns_topic" "alarms" {
  name = "${local.name}-alarms"

  tags = { Name = "${local.name}-alarms" }
}

resource "aws_sns_topic_subscription" "alarm_email" {
  count = var.alarm_email == "" ? 0 : 1

  topic_arn = aws_sns_topic.alarms.arn
  protocol  = "email"
  endpoint  = var.alarm_email
}

# Alarms are chosen to be actionable. Each one, if it fires, means something a
# person should do something about — an alarm that fires routinely and is
# ignored is worse than no alarm, because it trains people to ignore the others.

resource "aws_cloudwatch_metric_alarm" "http_5xx" {
  alarm_name        = "${local.name}-api-5xx"
  alarm_description = "The API is returning server errors. Check the api log group for the correlated request IDs."

  namespace   = "AWS/ApplicationELB"
  metric_name = "HTTPCode_Target_5XX_Count"
  statistic   = "Sum"
  period      = 300

  comparison_operator = "GreaterThanThreshold"
  threshold           = var.http_5xx_alarm_threshold
  evaluation_periods  = 1
  # Missing data means no traffic, which is not an error condition.
  treat_missing_data = "notBreaching"

  dimensions = {
    LoadBalancer = aws_lb.main.arn_suffix
    TargetGroup  = aws_lb_target_group.api.arn_suffix
  }

  alarm_actions = [aws_sns_topic.alarms.arn]
  ok_actions    = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "latency" {
  alarm_name        = "${local.name}-api-latency"
  alarm_description = "p99 response time is above target. Check RDS Performance Insights for slow queries first."

  namespace   = "AWS/ApplicationELB"
  metric_name = "TargetResponseTime"
  # p99, not average: an average hides the tail that users actually notice.
  extended_statistic  = "p99"
  period              = 300
  comparison_operator = "GreaterThanThreshold"
  threshold           = var.latency_alarm_threshold_seconds
  # Two periods, so a single slow window does not page anyone.
  evaluation_periods = 2
  treat_missing_data = "notBreaching"

  dimensions = {
    LoadBalancer = aws_lb.main.arn_suffix
    TargetGroup  = aws_lb_target_group.api.arn_suffix
  }

  alarm_actions = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "unhealthy_hosts" {
  alarm_name        = "${local.name}-api-unhealthy-hosts"
  alarm_description = "One or more API tasks are failing their readiness check."

  namespace           = "AWS/ApplicationELB"
  metric_name         = "UnHealthyHostCount"
  statistic           = "Maximum"
  period              = 60
  comparison_operator = "GreaterThanThreshold"
  threshold           = 0
  evaluation_periods  = 3
  treat_missing_data  = "notBreaching"

  dimensions = {
    LoadBalancer = aws_lb.main.arn_suffix
    TargetGroup  = aws_lb_target_group.api.arn_suffix
  }

  alarm_actions = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "rds_cpu" {
  alarm_name        = "${local.name}-rds-cpu"
  alarm_description = "Database CPU is sustained high. Usually an unindexed query or an export scanning more than it should."

  namespace           = "AWS/RDS"
  metric_name         = "CPUUtilization"
  statistic           = "Average"
  period              = 300
  comparison_operator = "GreaterThanThreshold"
  threshold           = 80
  evaluation_periods  = 2

  dimensions = {
    DBInstanceIdentifier = aws_db_instance.main.identifier
  }

  alarm_actions = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "rds_storage" {
  alarm_name        = "${local.name}-rds-free-storage"
  alarm_description = "Database free storage is low. Storage autoscaling should absorb this; if it fires, autoscaling is not keeping up."

  namespace           = "AWS/RDS"
  metric_name         = "FreeStorageSpace"
  statistic           = "Average"
  period              = 300
  comparison_operator = "LessThanThreshold"
  # 2 GiB, in bytes.
  threshold          = 2147483648
  evaluation_periods = 1

  dimensions = {
    DBInstanceIdentifier = aws_db_instance.main.identifier
  }

  alarm_actions = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "export_queue_depth" {
  alarm_name        = "${local.name}-export-queue-depth"
  alarm_description = "Usage exports are backing up. Scale the worker service or check the export log group for repeated failures."

  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 300
  comparison_operator = "GreaterThanThreshold"
  threshold           = var.export_queue_depth_alarm_threshold
  evaluation_periods  = 2
  treat_missing_data  = "notBreaching"

  dimensions = {
    QueueName = aws_sqs_queue.exports.name
  }

  alarm_actions = [aws_sns_topic.alarms.arn]
}

# Any message reaching the DLQ means a job failed its full retry budget, so the
# threshold is zero: this should never happen quietly.
resource "aws_cloudwatch_metric_alarm" "export_dlq" {
  alarm_name        = "${local.name}-export-dlq"
  alarm_description = "An export job exhausted its retries and was quarantined. Inspect the DLQ message and the export_jobs row."

  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 300
  comparison_operator = "GreaterThanThreshold"
  threshold           = 0
  evaluation_periods  = 1
  treat_missing_data  = "notBreaching"

  dimensions = {
    QueueName = aws_sqs_queue.export_dlq.name
  }

  alarm_actions = [aws_sns_topic.alarms.arn]
}

# A metric filter turns the application's own structured logs into a metric, so
# a spike in blocked cross-tenant access attempts is alarmable. This is the
# payoff for auditing denials that the caller only ever sees as a 404.
resource "aws_cloudwatch_log_metric_filter" "cross_tenant_denied" {
  name           = "${local.name}-cross-tenant-denied"
  log_group_name = aws_cloudwatch_log_group.api.name
  pattern        = "{ $.event = \"security.cross_tenant_denied\" }"

  metric_transformation {
    name      = "CrossTenantDenied"
    namespace = "BridgeCore/${var.environment}"
    value     = "1"
    unit      = "Count"
  }
}

resource "aws_cloudwatch_metric_alarm" "cross_tenant_denied" {
  alarm_name        = "${local.name}-cross-tenant-attempts"
  alarm_description = "A burst of blocked cross-tenant access attempts. Isolation held, but someone is enumerating: identify the tenant in the audit log."

  namespace           = "BridgeCore/${var.environment}"
  metric_name         = "CrossTenantDenied"
  statistic           = "Sum"
  period              = 300
  comparison_operator = "GreaterThanThreshold"
  threshold           = 20
  evaluation_periods  = 1
  treat_missing_data  = "notBreaching"

  alarm_actions = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_dashboard" "main" {
  dashboard_name = local.name

  dashboard_body = jsonencode({
    widgets = [
      {
        type = "metric", x = 0, y = 0, width = 12, height = 6
        properties = {
          title  = "Requests and errors"
          region = var.aws_region
          view   = "timeSeries"
          metrics = [
            ["AWS/ApplicationELB", "RequestCount", "LoadBalancer", aws_lb.main.arn_suffix, { stat = "Sum" }],
            [".", "HTTPCode_Target_5XX_Count", ".", ".", { stat = "Sum" }],
            [".", "HTTPCode_Target_4XX_Count", ".", ".", { stat = "Sum" }],
          ]
        }
      },
      {
        type = "metric", x = 12, y = 0, width = 12, height = 6
        properties = {
          title  = "Response time"
          region = var.aws_region
          view   = "timeSeries"
          metrics = [
            ["AWS/ApplicationELB", "TargetResponseTime", "LoadBalancer", aws_lb.main.arn_suffix, { stat = "p50" }],
            ["...", { stat = "p99" }],
          ]
        }
      },
      {
        type = "metric", x = 0, y = 6, width = 12, height = 6
        properties = {
          title  = "ECS utilisation"
          region = var.aws_region
          view   = "timeSeries"
          metrics = [
            ["AWS/ECS", "CPUUtilization", "ClusterName", aws_ecs_cluster.main.name, "ServiceName", aws_ecs_service.api.name],
            [".", "MemoryUtilization", ".", ".", ".", "."],
          ]
        }
      },
      {
        type = "metric", x = 12, y = 6, width = 12, height = 6
        properties = {
          title  = "Export pipeline"
          region = var.aws_region
          view   = "timeSeries"
          metrics = [
            ["AWS/SQS", "ApproximateNumberOfMessagesVisible", "QueueName", aws_sqs_queue.exports.name],
            [".", "ApproximateNumberOfMessagesVisible", "QueueName", aws_sqs_queue.export_dlq.name],
          ]
        }
      },
      {
        type = "metric", x = 0, y = 12, width = 12, height = 6
        properties = {
          title  = "Database"
          region = var.aws_region
          view   = "timeSeries"
          metrics = [
            ["AWS/RDS", "CPUUtilization", "DBInstanceIdentifier", aws_db_instance.main.identifier],
            [".", "DatabaseConnections", ".", "."],
          ]
        }
      },
      {
        type = "metric", x = 12, y = 12, width = 12, height = 6
        properties = {
          title  = "Tenant isolation denials"
          region = var.aws_region
          view   = "timeSeries"
          metrics = [
            ["BridgeCore/${var.environment}", "CrossTenantDenied", { stat = "Sum" }],
          ]
        }
      },
    ]
  })
}
