# IAM.
#
# Two roles per task, because they are used at different times by different
# principals:
#
#   execution role — used by the ECS agent BEFORE the container starts, to pull
#                    the image and resolve secrets into the environment.
#   task role      — used by the application code AT RUNTIME, to reach S3, SQS
#                    and Secrets Manager.
#
# Collapsing them into one role would hand the application the ability to pull
# arbitrary images and read every secret the agent can — permissions it has no
# use for.

data "aws_iam_policy_document" "ecs_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

# ---- Execution role ----

resource "aws_iam_role" "ecs_execution" {
  name               = "${local.name}-ecs-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json

  tags = { Name = "${local.name}-ecs-execution" }
}

resource "aws_iam_role_policy_attachment" "ecs_execution_managed" {
  role       = aws_iam_role.ecs_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

data "aws_iam_policy_document" "ecs_execution_secrets" {
  statement {
    sid     = "ReadInjectedSecrets"
    actions = ["secretsmanager:GetSecretValue"]
    # Scoped to exactly the two secrets this service injects, not to
    # secretsmanager:* — an execution role with wildcard secret access can read
    # every other service's credentials in the account.
    resources = [
      aws_secretsmanager_secret.app.arn,
      data.aws_secretsmanager_secret.rds.arn,
    ]
  }
}

resource "aws_iam_role_policy" "ecs_execution_secrets" {
  name   = "${local.name}-execution-secrets"
  role   = aws_iam_role.ecs_execution.id
  policy = data.aws_iam_policy_document.ecs_execution_secrets.json
}

# ---- Task role (application runtime) ----

resource "aws_iam_role" "ecs_task" {
  name               = "${local.name}-ecs-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json

  tags = { Name = "${local.name}-ecs-task" }
}

data "aws_iam_policy_document" "ecs_task" {
  statement {
    sid = "ExportObjectAccess"
    actions = [
      "s3:PutObject",
      "s3:GetObject",
      "s3:DeleteObject",
    ]
    # Object-level only, and only under the export prefix. The application has
    # no reason to list the bucket, and denying s3:ListBucket means a
    # compromised task cannot enumerate which tenants have exports — it can only
    # touch keys it already knows.
    resources = ["${aws_s3_bucket.exports.arn}/${local.export_prefix}/*"]
  }

  statement {
    sid = "ExportQueueAccess"
    actions = [
      "sqs:SendMessage",
      "sqs:ReceiveMessage",
      "sqs:DeleteMessage",
      "sqs:GetQueueAttributes",
      "sqs:GetQueueUrl",
    ]
    resources = [aws_sqs_queue.exports.arn]
  }

  statement {
    sid       = "ReadOwnSecrets"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.app.arn]
  }
}

resource "aws_iam_role_policy" "ecs_task" {
  name   = "${local.name}-task-policy"
  role   = aws_iam_role.ecs_task.id
  policy = data.aws_iam_policy_document.ecs_task.json
}

# ---- GitHub Actions deploy role (OIDC) ----
#
# No long-lived AWS access keys exist for CI. GitHub presents a short-lived
# OIDC token, AWS validates it against the trust policy below, and issues
# credentials that expire within the hour. A leaked repository secret would be
# usable until someone noticed; a leaked OIDC token is useless almost
# immediately.

resource "aws_iam_openid_connect_provider" "github" {
  count = var.create_github_oidc_provider ? 1 : 0

  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
  # GitHub's OIDC thumbprint. AWS no longer validates it for this provider, but
  # the field is still required.
  thumbprint_list = ["6938fd4d98bab03faadb97b34396831e3780aea1"]

  tags = { Name = "${local.name}-github-oidc" }
}

data "aws_iam_policy_document" "github_assume" {
  count = var.github_repository == "" ? 0 : 1

  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type = "Federated"
      identifiers = [
        var.create_github_oidc_provider
        ? aws_iam_openid_connect_provider.github[0].arn
        : "arn:aws:iam::${data.aws_caller_identity.current.account_id}:oidc-provider/token.actions.githubusercontent.com"
      ]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    # Scoped to this repository, and to the main branch specifically. Without
    # the sub condition, ANY GitHub repository in the world could assume this
    # role — this is the single most important line in the file.
    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values = [
        "repo:${var.github_repository}:ref:refs/heads/main",
        "repo:${var.github_repository}:environment:production",
      ]
    }
  }
}

resource "aws_iam_role" "github_deploy" {
  count = var.github_repository == "" ? 0 : 1

  name               = "${local.name}-github-deploy"
  assume_role_policy = data.aws_iam_policy_document.github_assume[0].json
  # Deployments are short; a one-hour ceiling limits how long a stolen session
  # remains useful.
  max_session_duration = 3600

  tags = { Name = "${local.name}-github-deploy" }
}

data "aws_iam_policy_document" "github_deploy" {
  count = var.github_repository == "" ? 0 : 1

  statement {
    sid       = "ECRAuth"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"] # this action does not support resource scoping
  }

  statement {
    sid = "ECRPush"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:CompleteLayerUpload",
      "ecr:InitiateLayerUpload",
      "ecr:PutImage",
      "ecr:UploadLayerPart",
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
      "ecr:DescribeImages",
    ]
    resources = [aws_ecr_repository.app.arn]
  }

  statement {
    sid = "ECSDeploy"
    actions = [
      "ecs:DescribeServices",
      "ecs:DescribeTaskDefinition",
      "ecs:RegisterTaskDefinition",
      "ecs:UpdateService",
      "ecs:DescribeTasks",
      "ecs:ListTasks",
    ]
    resources = ["*"]

    # Confine the pipeline to this cluster. Otherwise a compromised workflow
    # could redeploy unrelated services in the same account.
    condition {
      test     = "StringEquals"
      variable = "ecs:cluster"
      values   = [aws_ecs_cluster.main.arn]
    }
  }

  statement {
    sid       = "RegisterTaskDefinition"
    actions   = ["ecs:RegisterTaskDefinition", "ecs:DescribeTaskDefinition"]
    resources = ["*"] # task definition registration is not resource-scopable
  }

  statement {
    sid     = "PassTaskRoles"
    actions = ["iam:PassRole"]
    # Only these two roles. iam:PassRole with a wildcard is a full account
    # takeover: the pipeline could launch a task as any role, including admin.
    resources = [
      aws_iam_role.ecs_execution.arn,
      aws_iam_role.ecs_task.arn,
    ]

    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "github_deploy" {
  count = var.github_repository == "" ? 0 : 1

  name   = "${local.name}-github-deploy-policy"
  role   = aws_iam_role.github_deploy[0].id
  policy = data.aws_iam_policy_document.github_deploy[0].json
}

# ---- Export Lambda role ----

resource "aws_iam_role" "export_lambda" {
  count = local.enable_export_lambda ? 1 : 0

  name = "${local.name}-export-lambda"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "lambda.amazonaws.com" }
    }]
  })

  tags = { Name = "${local.name}-export-lambda" }
}

resource "aws_iam_role_policy_attachment" "export_lambda_vpc" {
  count = local.enable_export_lambda ? 1 : 0

  role       = aws_iam_role.export_lambda[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaVPCAccessExecutionRole"
}

resource "aws_iam_role_policy" "export_lambda" {
  count = local.enable_export_lambda ? 1 : 0

  name = "${local.name}-export-lambda-policy"
  role = aws_iam_role.export_lambda[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["s3:PutObject", "s3:GetObject"]
        Resource = "${aws_s3_bucket.exports.arn}/${local.export_prefix}/*"
      },
      {
        Effect = "Allow"
        Action = [
          "sqs:ReceiveMessage",
          "sqs:DeleteMessage",
          "sqs:GetQueueAttributes",
        ]
        Resource = aws_sqs_queue.exports.arn
      },
      {
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = [aws_secretsmanager_secret.app.arn, data.aws_secretsmanager_secret.rds.arn]
      },
    ]
  })
}
