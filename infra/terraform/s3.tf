# The export bucket holds generated CSVs, each of which contains one tenant's
# usage data. Access is exclusively via presigned URLs minted by the API after
# it has verified the requester owns the job.

resource "aws_s3_bucket" "exports" {
  bucket = "${local.name}-exports-${data.aws_caller_identity.current.account_id}"

  tags = { Name = "${local.name}-exports" }
}

resource "aws_s3_bucket_public_access_block" "exports" {
  bucket = aws_s3_bucket.exports.id

  # All four, unconditionally. A "temporarily public" export bucket is how
  # cross-tenant data leaks become news stories.
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "exports" {
  bucket = aws_s3_bucket.exports.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "exports" {
  bucket = aws_s3_bucket.exports.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_versioning" "exports" {
  bucket = aws_s3_bucket.exports.id

  versioning_configuration {
    # Exports are reproducible from the usage table, so versioning would only
    # multiply the storage of data that is already derived.
    status = "Suspended"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "exports" {
  bucket = aws_s3_bucket.exports.id

  rule {
    id     = "expire-generated-exports"
    status = "Enabled"

    filter {
      prefix = "usage-exports/"
    }

    # Deleting old exports bounds both the bill and the blast radius of a
    # future credential leak: there is simply less historical tenant data
    # sitting in the bucket to steal.
    expiration {
      days = var.export_object_expiration_days
    }
  }

  rule {
    id     = "abort-incomplete-uploads"
    status = "Enabled"

    filter {}

    # A worker killed mid-upload leaves a multipart upload consuming storage
    # that is invisible in the object listing.
    abort_incomplete_multipart_upload {
      days_after_initiation = 3
    }
  }
}

# Refuse any request that is not over TLS. Presigned URLs are capabilities; one
# fetched over plaintext HTTP is readable in transit.
resource "aws_s3_bucket_policy" "exports" {
  bucket = aws_s3_bucket.exports.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "DenyInsecureTransport"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:*"
        Resource = [
          aws_s3_bucket.exports.arn,
          "${aws_s3_bucket.exports.arn}/*",
        ]
        Condition = {
          Bool = { "aws:SecureTransport" = "false" }
        }
      }
    ]
  })

  depends_on = [aws_s3_bucket_public_access_block.exports]
}
