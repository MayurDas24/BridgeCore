resource "aws_lb" "main" {
  name               = "${local.name}-alb"
  internal           = false
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = aws_subnet.public[*].id

  # Longer than the application's own 60s write timeout so the ALB is not the
  # component that severs a slow-but-legitimate response. Exports are
  # asynchronous precisely so that no request ever approaches this.
  idle_timeout = 65

  drop_invalid_header_fields = true
  enable_deletion_protection = var.environment == "production"
  enable_http2               = true

  tags = { Name = "${local.name}-alb" }
}

resource "aws_lb_target_group" "api" {
  name        = "${local.name}-api-tg"
  port        = var.app_port
  protocol    = "HTTP"
  vpc_id      = aws_vpc.main.id
  target_type = "ip"

  # /ready, not /health and not /live. /ready reports whether this task can
  # serve traffic (its database and cache are reachable), which is exactly the
  # question a target group asks. /live would keep a task with a dead database
  # in rotation; /health returns extra diagnostics the ALB has no use for.
  health_check {
    enabled             = true
    path                = "/ready"
    protocol            = "HTTP"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  # Tasks must finish in-flight requests before the ALB stops sending them
  # traffic, or a deploy returns 502s to users mid-request.
  deregistration_delay = 30

  tags = { Name = "${local.name}-api-tg" }

  lifecycle {
    create_before_destroy = true
  }
}

# HTTP listener. With a certificate it does nothing but redirect; without one
# it serves traffic directly, which is acceptable only for a demo — bearer
# tokens over plaintext HTTP are readable by anything on the path.
resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"

  dynamic "default_action" {
    for_each = var.certificate_arn == "" ? [1] : []
    content {
      type             = "forward"
      target_group_arn = aws_lb_target_group.api.arn
    }
  }

  dynamic "default_action" {
    for_each = var.certificate_arn == "" ? [] : [1]
    content {
      type = "redirect"
      redirect {
        port        = "443"
        protocol    = "HTTPS"
        status_code = "HTTP_301"
      }
    }
  }
}

resource "aws_lb_listener" "https" {
  count = var.certificate_arn == "" ? 0 : 1

  load_balancer_arn = aws_lb.main.arn
  port              = 443
  protocol          = "HTTPS"
  # TLS 1.2 minimum: the policies that still allow TLS 1.0/1.1 exist for legacy
  # clients, and an API consumed by servers has none.
  ssl_policy      = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn = var.certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api.arn
  }
}
