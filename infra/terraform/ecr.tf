resource "aws_ecr_repository" "app" {
  name = var.project
  # Tags are immutable: a deployed tag must always mean the same bytes. With
  # mutable tags, "roll back to the previous SHA" can silently pull a different
  # image than the one that was tested.
  image_tag_mutability = "IMMUTABLE"
  force_delete         = false

  image_scanning_configuration {
    # Scan every push, so a base-image CVE is found at build time rather than
    # by an auditor months later.
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }

  tags = { Name = "${local.name}-ecr" }
}

resource "aws_ecr_lifecycle_policy" "app" {
  repository = aws_ecr_repository.app.name

  # Every commit to main pushes an image. Without expiry the repository grows
  # without bound; 30 tagged images is comfortably more history than a rollback
  # ever needs.
  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Expire untagged images after 1 day"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 1
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 2
        description  = "Keep the 30 most recent images"
        selection = {
          tagStatus   = "any"
          countType   = "imageCountMoreThan"
          countNumber = 30
        }
        action = { type = "expire" }
      }
    ]
  })
}
