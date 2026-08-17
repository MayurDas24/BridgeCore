terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  # State is commented out so `terraform init -backend=false` works in CI and
  # so a first-time reader can plan without provisioning a bucket first.
  # Uncomment for any shared or production use: local state cannot be locked,
  # so two people (or a pipeline and a person) applying at once will corrupt it.
  #
  # backend "s3" {
  #   bucket       = "bridgecore-terraform-state"
  #   key          = "production/terraform.tfstate"
  #   region       = "ap-south-1"
  #   encrypt      = true
  #   use_lockfile = true
  # }
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = local.common_tags
  }
}
