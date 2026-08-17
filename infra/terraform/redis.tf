resource "aws_elasticache_subnet_group" "main" {
  name       = "${local.name}-redis-subnets"
  subnet_ids = aws_subnet.private[*].id

  tags = { Name = "${local.name}-redis-subnets" }
}

resource "aws_elasticache_parameter_group" "main" {
  name   = "${local.name}-redis7"
  family = "redis7"

  # Redis here holds rate-limit counters and nothing durable, so evicting the
  # oldest keys under memory pressure is strictly better than refusing writes —
  # a failed rate-limit write would otherwise reject a legitimate request.
  parameter {
    name  = "maxmemory-policy"
    value = "volatile-lru"
  }
}

resource "aws_elasticache_replication_group" "main" {
  replication_group_id = "${local.name}-redis"
  description          = "BridgeCore rate limiting and caching"

  engine               = "redis"
  engine_version       = var.redis_engine_version
  node_type            = var.redis_node_type
  parameter_group_name = aws_elasticache_parameter_group.main.name
  port                 = 6379

  subnet_group_name  = aws_elasticache_subnet_group.main.name
  security_group_ids = [aws_security_group.redis.id]

  num_cache_clusters         = var.db_multi_az ? 2 : 1
  automatic_failover_enabled = var.db_multi_az
  multi_az_enabled           = var.db_multi_az

  at_rest_encryption_enabled = true
  # In-transit encryption is why the application exposes REDIS_TLS: the client
  # must be told to speak TLS, or every command fails with a protocol error.
  transit_encryption_enabled = true

  snapshot_retention_limit = 0 # nothing here is worth restoring
  apply_immediately        = var.environment != "production"

  tags = { Name = "${local.name}-redis" }

  lifecycle {
    ignore_changes = [engine_version]
  }
}
