// Package database owns connection lifecycle for PostgreSQL and Redis.
// BridgeCore intentionally uses database/sql + lib/pq rather than an ORM:
// at platform-engineering scale, hand-written SQL in the repository layer
// gives full control over query plans, indexes, and transactions, and
// keeps the dependency surface small and auditable.
package database

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/bridgecore/bridgecore/internal/config"
)

// DB wraps *sql.DB with a couple of convenience accessors.
type DB struct {
	*sql.DB
}

// Redis wraps the redis client.
type Redis struct {
	*redis.Client
}

// NewPostgres opens and validates a PostgreSQL connection pool according to
// the supplied configuration.
func NewPostgres(cfg config.DatabaseConfig) (*DB, error) {
	sqlDB, err := sql.Open("postgres", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("database: open postgres: %w", err)
	}

	// Connection pool sizing is configuration, not a constant: RDS enforces a
	// hard max_connections, and the pool ceiling multiplied by the number of
	// ECS tasks must stay under it or new tasks fail to connect during a
	// scale-out.
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	// Recycling idle connections matters behind RDS Proxy and after a failover,
	// where a connection can remain open but point at a demoted instance.
	sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("database: ping postgres: %w", err)
	}

	return &DB{sqlDB}, nil
}

// NewRedis opens a Redis client and validates connectivity with a PING.
//
// TLS is configurable because ElastiCache with in-transit encryption enabled
// requires it, while a local Docker Redis does not offer it at all.
func NewRedis(cfg config.RedisConfig) (*Redis, error) {
	opts := &redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		// Redis backs rate limiting, which is on the hot path of every
		// request, so the pool is sized to absorb bursts rather than serialize
		// them behind a handful of connections.
		PoolSize:     20,
		MinIdleConns: 5,
	}
	if cfg.TLS {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("database: ping redis: %w", err)
	}

	return &Redis{client}, nil
}

// Healthy reports whether the Postgres connection is currently reachable.
func (d *DB) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return d.PingContext(ctx) == nil
}

// Healthy reports whether the Redis connection is currently reachable.
func (r *Redis) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return r.Ping(ctx).Err() == nil
}
