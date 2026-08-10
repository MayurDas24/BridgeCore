// Package database owns connection lifecycle for PostgreSQL and Redis.
// BridgeCore intentionally uses database/sql + lib/pq rather than an ORM:
// at platform-engineering scale, hand-written SQL in the repository layer
// gives full control over query plans, indexes, and transactions, and
// keeps the dependency surface small and auditable.
package database

import (
	"context"
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

	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("database: ping postgres: %w", err)
	}

	return &DB{sqlDB}, nil
}

// NewRedis opens a Redis client and validates connectivity with a PING.
func NewRedis(addr, password string, db int) (*Redis, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

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
