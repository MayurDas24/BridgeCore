# Application secrets.
#
# The task definition references only the ARN below, never a value. The task
# role is permitted to read it, and the application loads it at boot
# (cmd/api/bootstrap.go). Consequences worth stating plainly: a leaked task
# definition contains no credentials, a leaked image contains no credentials,
# and rotating a secret does not require rebuilding or re-registering anything.

resource "random_password" "jwt_access" {
  length  = 64
  special = false
}

resource "random_password" "jwt_refresh" {
  length  = 64
  special = false
}

resource "random_password" "platform_admin_token" {
  length  = 48
  special = false
}

resource "random_password" "export_signing_key" {
  length  = 48
  special = false
}

resource "aws_secretsmanager_secret" "app" {
  name        = "${local.name}/app"
  description = "BridgeCore application configuration secrets"

  # Long enough to recover from an accidental delete, short enough that a
  # rotated-out secret does not linger indefinitely.
  recovery_window_in_days = 7

  tags = { Name = "${local.name}-app-secrets" }
}

resource "aws_secretsmanager_secret_version" "app" {
  secret_id = aws_secretsmanager_secret.app.id

  # A flat JSON object of environment variable names, which is exactly what
  # config.ApplySecrets expects. The application's configuration contract is
  # therefore identical locally (.env) and in production (this secret).
  secret_string = jsonencode({
    JWT_ACCESS_SECRET    = random_password.jwt_access.result
    JWT_REFRESH_SECRET   = random_password.jwt_refresh.result
    PLATFORM_ADMIN_TOKEN = random_password.platform_admin_token.result
    EXPORT_SIGNING_KEY   = random_password.export_signing_key.result
  })

  lifecycle {
    # Terraform generated the initial values; after that, rotation happens in
    # Secrets Manager. Without this, every apply would reset the secrets and
    # invalidate every issued token.
    ignore_changes = [secret_string]
  }
}

# The RDS master credentials are managed by RDS itself
# (manage_master_user_password), which creates and rotates its own secret. This
# reads it so the task definition can inject the password without Terraform
# ever handling the value.
data "aws_secretsmanager_secret" "rds" {
  arn = aws_db_instance.main.master_user_secret[0].secret_arn
}
