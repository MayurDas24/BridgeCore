resource "aws_ecs_cluster" "main" {
  name = "${local.name}-cluster"

  setting {
    name  = "containerInsights"
    value = "enabled"
  }

  tags = { Name = "${local.name}-cluster" }
}

resource "aws_ecs_cluster_capacity_providers" "main" {
  cluster_name       = aws_ecs_cluster.main.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
  }
}

# Environment shared by the API and the worker. Values here are non-sensitive
# by construction; anything secret is injected from Secrets Manager below.
locals {
  common_environment = [
    { name = "APP_ENV", value = var.environment },
    { name = "APP_PORT", value = tostring(var.app_port) },
    { name = "APP_VERSION", value = var.image_tag },
    { name = "DB_HOST", value = aws_db_instance.main.address },
    { name = "DB_PORT", value = tostring(aws_db_instance.main.port) },
    { name = "DB_NAME", value = var.db_name },
    { name = "DB_USER", value = var.db_username },
    # The RDS parameter group forces TLS, so anything less fails to connect.
    { name = "DB_SSLMODE", value = "require" },
    { name = "REDIS_ADDR", value = "${aws_elasticache_replication_group.main.primary_endpoint_address}:6379" },
    { name = "REDIS_TLS", value = "true" },
    { name = "AWS_REGION", value = var.aws_region },
    { name = "EXPORT_BACKEND", value = "s3" },
    { name = "EXPORT_S3_BUCKET", value = aws_s3_bucket.exports.bucket },
    { name = "EXPORT_S3_PREFIX", value = local.export_prefix },
    { name = "EXPORT_SQS_QUEUE_URL", value = aws_sqs_queue.exports.url },
    { name = "EXPORT_DOWNLOAD_TTL", value = var.export_url_ttl },
    { name = "CORS_ALLOWED_ORIGINS", value = var.cors_allowed_origins },
    # Both must be off in production; the application refuses to start otherwise.
    { name = "GRAPHQL_PLAYGROUND", value = "false" },
    { name = "GRAPHQL_INTROSPECTION", value = "false" },
    { name = "EXPOSE_DEV_TOOLS", value = "false" },
  ]

  # Injected by the ECS agent before the container starts. The task definition
  # stores ARNs, never values, so `aws ecs describe-task-definition` reveals
  # nothing.
  common_secrets = [
    { name = "JWT_ACCESS_SECRET", valueFrom = "${aws_secretsmanager_secret.app.arn}:JWT_ACCESS_SECRET::" },
    { name = "JWT_REFRESH_SECRET", valueFrom = "${aws_secretsmanager_secret.app.arn}:JWT_REFRESH_SECRET::" },
    { name = "PLATFORM_ADMIN_TOKEN", valueFrom = "${aws_secretsmanager_secret.app.arn}:PLATFORM_ADMIN_TOKEN::" },
    { name = "EXPORT_SIGNING_KEY", valueFrom = "${aws_secretsmanager_secret.app.arn}:EXPORT_SIGNING_KEY::" },
    { name = "DB_PASSWORD", valueFrom = "${data.aws_secretsmanager_secret.rds.arn}:password::" },
  ]
}

# ---- API service ----

resource "aws_ecs_task_definition" "api" {
  family                   = "${local.name}-api"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.api_cpu)
  memory                   = tostring(var.api_memory)
  execution_role_arn       = aws_iam_role.ecs_execution.arn
  task_role_arn            = aws_iam_role.ecs_task.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }

  container_definitions = jsonencode([
    {
      name      = "${var.project}-api"
      image     = "${aws_ecr_repository.app.repository_url}:${var.image_tag}"
      essential = true

      portMappings = [{
        containerPort = var.app_port
        protocol      = "tcp"
      }]

      environment = concat(local.common_environment, [
        # The API owns migrations and runs them on boot; the worker must not.
        { name = "EXPORT_IN_PROCESS_WORKER", value = "false" },
      ])
      secrets = local.common_secrets

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.api.name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "api"
        }
      }

      # No container-level healthCheck. A distroless image has no shell, and
      # every ECS health check form requires one to run the command. Health is
      # therefore judged by the ALB target group polling /ready, which is the
      # signal that actually controls whether a task receives traffic.

      stopTimeout = 30
    }
  ])

  tags = { Name = "${local.name}-api-task" }
}

resource "aws_ecs_service" "api" {
  name            = "${var.project}-api"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.api.arn
  desired_count   = var.api_desired_count
  launch_type     = "FARGATE"

  # Replace tasks without ever dropping below the current capacity: 200/100
  # means "start the new ones, then retire the old ones".
  deployment_maximum_percent         = 200
  deployment_minimum_healthy_percent = 100

  # The safety net that makes automated deployment defensible. If the new task
  # set never becomes healthy, ECS reverts to the previous one by itself —
  # before the pipeline's own rollback step is even reached.
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = aws_subnet.private[*].id
    security_groups  = [aws_security_group.ecs_tasks.id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.api.arn
    container_name   = "${var.project}-api"
    container_port   = var.app_port
  }

  # Give a task time to run migrations and warm up before its failing health
  # checks count against it.
  health_check_grace_period_seconds = 60

  enable_execute_command = var.environment != "production"

  lifecycle {
    # The deploy pipeline sets the task definition and desired count; Terraform
    # must not fight it and roll production back to the last applied image.
    ignore_changes = [task_definition, desired_count]
  }

  depends_on = [aws_lb_listener.http]

  tags = { Name = "${local.name}-api-service" }
}

# ---- Autoscaling ----

resource "aws_appautoscaling_target" "api" {
  service_namespace  = "ecs"
  resource_id        = "service/${aws_ecs_cluster.main.name}/${aws_ecs_service.api.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  min_capacity       = var.api_min_capacity
  max_capacity       = var.api_max_capacity
}

resource "aws_appautoscaling_policy" "api_cpu" {
  name               = "${local.name}-api-cpu"
  policy_type        = "TargetTrackingScaling"
  service_namespace  = aws_appautoscaling_target.api.service_namespace
  resource_id        = aws_appautoscaling_target.api.resource_id
  scalable_dimension = aws_appautoscaling_target.api.scalable_dimension

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
    target_value = 65
    # Scale out quickly, scale in slowly: a premature scale-in during a traffic
    # spike causes the exact latency the scaling exists to prevent.
    scale_in_cooldown  = 300
    scale_out_cooldown = 60
  }
}

resource "aws_appautoscaling_policy" "api_requests" {
  name               = "${local.name}-api-requests"
  policy_type        = "TargetTrackingScaling"
  service_namespace  = aws_appautoscaling_target.api.service_namespace
  resource_id        = aws_appautoscaling_target.api.resource_id
  scalable_dimension = aws_appautoscaling_target.api.scalable_dimension

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ALBRequestCountPerTarget"
      resource_label         = "${aws_lb.main.arn_suffix}/${aws_lb_target_group.api.arn_suffix}"
    }
    # Request-rate scaling reacts before CPU does for an I/O-bound API, where
    # tasks queue on the database long before they saturate a core.
    target_value       = 800
    scale_in_cooldown  = 300
    scale_out_cooldown = 60
  }
}

# ---- Export worker service ----

resource "aws_ecs_task_definition" "worker" {
  family                   = "${local.name}-worker"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.worker_cpu)
  memory                   = tostring(var.worker_memory)
  execution_role_arn       = aws_iam_role.ecs_execution.arn
  task_role_arn            = aws_iam_role.ecs_task.arn

  container_definitions = jsonencode([
    {
      name      = "${var.project}-worker"
      image     = "${aws_ecr_repository.app.repository_url}:${var.image_tag}"
      essential = true
      # Same image, different entrypoint: one artifact to build, scan and
      # promote, so the worker can never be running different business logic
      # than the API.
      entryPoint = ["/app/bridgecore-worker"]

      environment = local.common_environment
      secrets     = local.common_secrets

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.worker.name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "worker"
        }
      }

      # Generous, because SIGTERM lets the worker finish the export it is
      # holding rather than abandoning a half-written object.
      stopTimeout = 120
    }
  ])

  tags = { Name = "${local.name}-worker-task" }
}

resource "aws_ecs_service" "worker" {
  name            = "${var.project}-worker"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.worker.arn
  desired_count   = var.worker_desired_count
  launch_type     = "FARGATE"

  # No load balancer and no minimum healthy percent of 100: the worker serves no
  # traffic, so replacing it outright is fine. Jobs claimed by a task that dies
  # are reclaimed by another after the visibility timeout.
  deployment_minimum_healthy_percent = 0
  deployment_maximum_percent         = 200

  network_configuration {
    subnets          = aws_subnet.private[*].id
    security_groups  = [aws_security_group.ecs_tasks.id]
    assign_public_ip = false
  }

  lifecycle {
    ignore_changes = [task_definition, desired_count]
  }

  tags = { Name = "${local.name}-worker-service" }
}
