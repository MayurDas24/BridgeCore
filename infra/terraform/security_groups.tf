# Security groups are the real isolation boundary between tiers.
#
# Every rule below references another security group rather than a CIDR block.
# That matters: "allow 5432 from the ECS task security group" stays correct
# when tasks are replaced, rescheduled into a different AZ, or scaled out,
# whereas "allow 5432 from 10.20.0.0/16" quietly permits anything that ever
# lands in the VPC — including a future unrelated workload.

resource "aws_security_group" "alb" {
  name        = "${local.name}-alb-sg"
  description = "Public entry point. The only group that accepts traffic from the internet."
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-alb-sg" }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "alb_https" {
  count = var.certificate_arn == "" ? 0 : length(var.alb_ingress_cidrs)

  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from permitted networks"
  cidr_ipv4         = var.alb_ingress_cidrs[count.index]
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
}

resource "aws_vpc_security_group_ingress_rule" "alb_http" {
  count = length(var.alb_ingress_cidrs)

  security_group_id = aws_security_group.alb.id
  # With a certificate this port only serves the redirect to HTTPS; without
  # one it is the actual listener, which is why the app also sets HSTS-adjacent
  # security headers and why running without a certificate is documented as
  # demo-only.
  description = "HTTP from permitted networks (redirects to HTTPS when a certificate is configured)"
  cidr_ipv4   = var.alb_ingress_cidrs[count.index]
  from_port   = 80
  to_port     = 80
  ip_protocol = "tcp"
}

resource "aws_vpc_security_group_egress_rule" "alb_to_tasks" {
  security_group_id            = aws_security_group.alb.id
  description                  = "Forward to application tasks only"
  referenced_security_group_id = aws_security_group.ecs_tasks.id
  from_port                    = var.app_port
  to_port                      = var.app_port
  ip_protocol                  = "tcp"
}

resource "aws_security_group" "ecs_tasks" {
  name        = "${local.name}-ecs-tasks-sg"
  description = "API and worker tasks. Reachable only from the load balancer."
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-ecs-tasks-sg" }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "tasks_from_alb" {
  security_group_id            = aws_security_group.ecs_tasks.id
  description                  = "Application port, from the load balancer only"
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = var.app_port
  to_port                      = var.app_port
  ip_protocol                  = "tcp"
}

# Tasks need unrestricted egress: pulling images from ECR, reaching Secrets
# Manager and SQS, and writing to S3. Restricting this to a curated list of
# AWS prefix lists is possible but brittle, and the tasks have no inbound path
# from the internet, so outbound is not the boundary that matters here.
resource "aws_vpc_security_group_egress_rule" "tasks_all" {
  security_group_id = aws_security_group.ecs_tasks.id
  description       = "Outbound to AWS services and the internet via NAT"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_security_group" "rds" {
  name        = "${local.name}-rds-sg"
  description = "PostgreSQL. Reachable only from application tasks."
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-rds-sg" }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "rds_from_tasks" {
  security_group_id            = aws_security_group.rds.id
  description                  = "PostgreSQL from application tasks"
  referenced_security_group_id = aws_security_group.ecs_tasks.id
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
}

resource "aws_vpc_security_group_ingress_rule" "rds_from_lambda" {
  count = local.enable_export_lambda ? 1 : 0

  security_group_id            = aws_security_group.rds.id
  description                  = "PostgreSQL from the export Lambda"
  referenced_security_group_id = aws_security_group.lambda[0].id
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
}

# No egress rules at all: the database has no legitimate reason to originate a
# connection, and denying it removes a data-exfiltration path.

resource "aws_security_group" "redis" {
  name        = "${local.name}-redis-sg"
  description = "ElastiCache Redis. Reachable only from application tasks."
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-redis-sg" }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "redis_from_tasks" {
  security_group_id            = aws_security_group.redis.id
  description                  = "Redis from application tasks"
  referenced_security_group_id = aws_security_group.ecs_tasks.id
  from_port                    = 6379
  to_port                      = 6379
  ip_protocol                  = "tcp"
}

resource "aws_security_group" "lambda" {
  count = local.enable_export_lambda ? 1 : 0

  name        = "${local.name}-lambda-sg"
  description = "Export consumer Lambda, placed in private subnets to reach RDS."
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-lambda-sg" }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_egress_rule" "lambda_all" {
  count = local.enable_export_lambda ? 1 : 0

  security_group_id = aws_security_group.lambda[0].id
  description       = "Outbound to RDS, S3 and SQS"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}
