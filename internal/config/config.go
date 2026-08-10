// Package config centralizes all environment-driven configuration for BridgeCore.
// Every tunable value used across the platform is resolved here, once, at boot,
// so the rest of the codebase never touches os.Getenv directly.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

// Config is the fully-resolved application configuration.
type Config struct {
	Env  string // "development", "staging", "production"
	Port string

	DB DatabaseConfig

	RedisAddr     string
	RedisPassword string
	RedisDB       int

	JWT JWTConfig

	APIKeyPrefix string

	RateLimitRequestsPerMinute int

	// Build metadata, injected at build time via -ldflags. Defaults are used
	// when running via `go run` in local development.
	Version   string
	BuildTime string
}

// DatabaseConfig holds PostgreSQL connection settings.
type DatabaseConfig struct {
	Host            string
	Port            string
	User            string
	Password        string
	Name            string
	SSLMode         string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// DSN builds a PostgreSQL connection string from the database config.
func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.Name, d.SSLMode,
	)
}

// JWTConfig holds signing secrets and token lifetimes.
type JWTConfig struct {
	AccessSecret    string
	RefreshSecret   string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	Issuer          string
}

// Load reads a .env file (if present) and then resolves configuration from
// the process environment, applying sane defaults for local development.
// In production, the .env file will not exist and real environment
// variables (injected by the orchestrator) are used instead.
func Load() (*Config, error) {
	_ = godotenv.Load() // ignore error: .env is optional (e.g. in containers)

	cfg := &Config{
		Env:  getEnv("APP_ENV", "development"),
		Port: getEnv("APP_PORT", "8080"),

		DB: DatabaseConfig{
			Host:            getEnv("DB_HOST", "localhost"),
			Port:            getEnv("DB_PORT", "5432"),
			User:            getEnv("DB_USER", "bridgecore"),
			Password:        getEnv("DB_PASSWORD", "bridgecore"),
			Name:            getEnv("DB_NAME", "bridgecore"),
			SSLMode:         getEnv("DB_SSLMODE", "disable"),
			MaxOpenConns:    getEnvInt("DB_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    getEnvInt("DB_MAX_IDLE_CONNS", 10),
			ConnMaxLifetime: getEnvDuration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		},

		RedisAddr:     getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),
		RedisDB:       getEnvInt("REDIS_DB", 0),

		JWT: JWTConfig{
			AccessSecret:    getEnv("JWT_ACCESS_SECRET", "dev-access-secret-change-me"),
			RefreshSecret:   getEnv("JWT_REFRESH_SECRET", "dev-refresh-secret-change-me"),
			AccessTokenTTL:  getEnvDuration("JWT_ACCESS_TTL", 15*time.Minute),
			RefreshTokenTTL: getEnvDuration("JWT_REFRESH_TTL", 7*24*time.Hour),
			Issuer:          getEnv("JWT_ISSUER", "bridgecore"),
		},

		APIKeyPrefix: getEnv("API_KEY_PREFIX", "bc_live_"),

		RateLimitRequestsPerMinute: getEnvInt("RATE_LIMIT_RPM", 120),

		Version:   getEnv("APP_VERSION", "0.1.0-dev"),
		BuildTime: getEnv("APP_BUILD_TIME", "unknown"),
	}

	if cfg.Env == "production" {
		if cfg.JWT.AccessSecret == "dev-access-secret-change-me" ||
			cfg.JWT.RefreshSecret == "dev-refresh-secret-change-me" {
			return nil, fmt.Errorf("config: refusing to start in production with default JWT secrets")
		}
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
