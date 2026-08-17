resource "aws_db_subnet_group" "main" {
  name       = "${local.name}-db-subnets"
  subnet_ids = aws_subnet.private[*].id

  tags = { Name = "${local.name}-db-subnets" }
}

resource "aws_db_parameter_group" "main" {
  name   = "${local.name}-pg16"
  family = "postgres16"

  # Log any statement slower than a second. This is how the N+1 queries the
  # DataLoader exists to prevent get caught in production rather than guessed at.
  parameter {
    name  = "log_min_duration_statement"
    value = "1000"
  }

  parameter {
    name  = "log_connections"
    value = "1"
  }

  # Require TLS for every client connection. The application sets
  # DB_SSLMODE=require to match, and config validation refuses to start
  # production with sslmode=disable — this makes the database enforce it too,
  # rather than trusting the client to ask.
  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_db_instance" "main" {
  identifier     = "${local.name}-postgres"
  engine         = "postgres"
  engine_version = "16"
  instance_class = var.db_instance_class

  allocated_storage     = var.db_allocated_storage
  max_allocated_storage = var.db_max_allocated_storage
  storage_type          = "gp3"
  storage_encrypted     = true

  db_name  = var.db_name
  username = var.db_username
  # Managed in Secrets Manager and rotated there, so the password never appears
  # in Terraform state as a plaintext variable, in a task definition, or in a
  # CI log.
  manage_master_user_password = true

  db_subnet_group_name   = aws_db_subnet_group.main.name
  vpc_security_group_ids = [aws_security_group.rds.id]
  parameter_group_name   = aws_db_parameter_group.main.name
  # The instance sits in subnets with no internet route, so this is belt and
  # braces rather than the only control.
  publicly_accessible = false

  multi_az                = var.db_multi_az
  backup_retention_period = var.db_backup_retention_days
  backup_window           = "17:00-18:00"
  maintenance_window      = "Sun:18:30-Sun:19:30"
  copy_tags_to_snapshot   = true

  # Take a final snapshot on destroy: an accidental `terraform destroy` should
  # cost a restore, not the data.
  skip_final_snapshot       = false
  final_snapshot_identifier = "${local.name}-postgres-final"
  deletion_protection       = var.db_deletion_protection

  auto_minor_version_upgrade      = true
  performance_insights_enabled    = true
  performance_insights_retention_period = 7
  enabled_cloudwatch_logs_exports = ["postgresql", "upgrade"]

  tags = { Name = "${local.name}-postgres" }

  lifecycle {
    # Version drift from an automatic minor upgrade must not make Terraform
    # want to downgrade the engine on the next apply.
    ignore_changes = [engine_version]
  }
}
