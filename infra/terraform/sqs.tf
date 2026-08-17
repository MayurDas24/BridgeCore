# The export queue is a notification channel, not the system of record.
#
# Job state lives in the export_jobs table in PostgreSQL, claimed with
# FOR UPDATE SKIP LOCKED. SQS exists so a consumer starts work immediately
# instead of waiting for the worker's next poll. If the queue is unavailable,
# or a message is lost, the job is still in the table and still gets processed —
# the export is late, not lost. Making the queue authoritative would mean
# reconciling two sources of truth on every failure.

resource "aws_sqs_queue" "export_dlq" {
  name = "${local.name}-exports-dlq"

  # Long retention: a message here represents a job that failed repeatedly, and
  # the whole point is to still have it when someone investigates next week.
  message_retention_seconds = 1209600 # 14 days
  sqs_managed_sse_enabled   = true

  tags = { Name = "${local.name}-exports-dlq" }
}

resource "aws_sqs_queue" "exports" {
  name = "${local.name}-exports"

  # Must exceed the consumer's worst-case runtime, or a slow export is
  # redelivered while it is still being generated.
  visibility_timeout_seconds = 900
  message_retention_seconds  = 86400
  # Long polling: the consumer waits on the queue rather than spinning on empty
  # receives, which is both cheaper and lower latency.
  receive_wait_time_seconds = 20
  sqs_managed_sse_enabled   = true

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.export_dlq.arn
    # After three failed attempts the message is quarantined instead of
    # retrying forever, which is how a single poison message consumes an entire
    # worker fleet.
    maxReceiveCount = 3
  })

  tags = { Name = "${local.name}-exports" }
}

resource "aws_sqs_queue_redrive_allow_policy" "export_dlq" {
  queue_url = aws_sqs_queue.export_dlq.id

  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.exports.arn]
  })
}
