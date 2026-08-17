# Optional serverless export consumer.
#
# The worker ECS service and this Lambda are alternative consumers of the same
# queue and the same export_jobs table. Which one you run is an operational
# choice, not an architectural one:
#
#   worker service — steady export volume, predictable cost, no cold starts,
#                    holds a warm database connection pool.
#   Lambda         — bursty or infrequent volume, scales to zero, pays only per
#                    export, but pays a cold start and opens a fresh database
#                    connection per invocation.
#
# Both are safe to run simultaneously because claiming is done with
# FOR UPDATE SKIP LOCKED in PostgreSQL: whichever consumer claims a job first
# owns it, and the other simply finds nothing to do.

resource "aws_lambda_function" "export" {
  count = local.enable_export_lambda ? 1 : 0

  function_name = "${local.name}-usage-export"
  role          = aws_iam_role.export_lambda[0].arn

  # provided.al2023 with a Go binary named "bootstrap": Go has no managed
  # runtime, so it ships as a custom runtime.
  runtime       = "provided.al2023"
  handler       = "bootstrap"
  architectures = ["x86_64"]

  filename         = var.export_lambda_zip_path
  source_code_hash = filebase64sha256(var.export_lambda_zip_path)

  # Long enough for a large export, comfortably under the queue's 900s
  # visibility timeout so a message is never redelivered while still running.
  timeout = 600
  # Exports stream row-by-row to a temp file rather than buffering, so memory
  # does not scale with row count. This is sized for CSV encoding overhead.
  memory_size = 512

  # A single concurrent execution by default: exports are database-heavy, and an
  # unbounded Lambda fan-out is the fastest way to exhaust the RDS connection
  # limit and take the API down with it.
  reserved_concurrent_executions = 2

  vpc_config {
    subnet_ids         = aws_subnet.private[*].id
    security_group_ids = [aws_security_group.lambda[0].id]
  }

  environment {
    variables = {
      APP_ENV                       = var.environment
      DB_HOST                       = aws_db_instance.main.address
      DB_PORT                       = tostring(aws_db_instance.main.port)
      DB_NAME                       = var.db_name
      DB_USER                       = var.db_username
      DB_SSLMODE                    = "require"
      AWS_SECRETS_MANAGER_SECRET_ID = aws_secretsmanager_secret.app.arn
      RDS_SECRET_ID                 = data.aws_secretsmanager_secret.rds.arn
      EXPORT_BACKEND                = "s3"
      EXPORT_S3_BUCKET              = aws_s3_bucket.exports.bucket
      EXPORT_S3_PREFIX              = local.export_prefix
      EXPORT_MAX_ROWS               = "500000"
    }
  }

  logging_config {
    log_format = "JSON"
    log_group  = aws_cloudwatch_log_group.export_lambda[0].name
  }

  tags = { Name = "${local.name}-usage-export" }

  depends_on = [aws_cloudwatch_log_group.export_lambda]
}

resource "aws_lambda_event_source_mapping" "export" {
  count = local.enable_export_lambda ? 1 : 0

  event_source_arn = aws_sqs_queue.exports.arn
  function_name    = aws_lambda_function.export[0].arn

  # One job per invocation: batching would mean a single failure re-delivers
  # every job in the batch, and exports are long enough that partial batch
  # failure handling is not worth the complexity.
  batch_size                         = 1
  maximum_batching_window_in_seconds = 0

  function_response_types = ["ReportBatchItemFailures"]
}
