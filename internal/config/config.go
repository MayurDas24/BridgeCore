// Package config centralizes all environment-driven configuration for BridgeCore.
// Every tunable value used across the platform is resolved here, once, at boot,
// so the rest of the codebase never touches os.Getenv directly.
//
// Configuration is deliberately fail-fast: Load resolves values, then
// Validate refuses to return a Config that would be unsafe to serve
// production traffic with (default JWT secrets, wildcard CORS, unencrypted
// database connections). A process that cannot be configured safely should
// never reach ListenAndServe.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Environment names used across the platform.
const (
	EnvDevelopment = "development"
	EnvStaging     = "staging"
	EnvProduction  = "production"
)

// Sentinel default secrets. These exist so local development works with
// zero setup, and so Validate can detect that they were never replaced.
const (
	defaultAccessSecret  = "dev-access-secret-change-me"
	defaultRefreshSecret = "dev-refresh-secret-change-me"
)

// minProductionSecretLength is the shortest JWT signing secret accepted in
// production. 32 bytes is the output size of the HS256 HMAC, so anything
// shorter reduces the effective key strength below the algorithm's.
const minProductionSecretLength = 32

// Config is the fully-resolved application configuration.
type Config struct {
	Env  string // "development", "staging", "production"
	Port string

	DB    DatabaseConfig
	Redis RedisConfig
	JWT   JWTConfig

	CORS    CORSConfig
	GraphQL GraphQLConfig
	Exports ExportConfig
	AWS     AWSConfig

	APIKeyPrefix string

	// PlatformAdminToken authenticates the platform control-plane routes
	// (cross-tenant provisioning, plan changes, entitlement grants). It is a
	// deliberately separate credential from any tenant's JWT: no
	// tenant-scoped token, however privileged inside its own tenant, can
	// ever reach a cross-tenant operation. When empty, the platform routes
	// are not registered at all.
	PlatformAdminToken string

	RateLimitRequestsPerMinute int

	// MaxPageSize is the hard ceiling on any paginated list, for both REST
	// and GraphQL. Clients cannot request unbounded data.
	MaxPageSize     int
	DefaultPageSize int

	// ExposeDevTools controls whether developer-only surfaces (Swagger UI,
	// the GraphQL playground) are served. Defaults to false in production.
	ExposeDevTools bool

	// Build metadata, injected at build time via -ldflags. Defaults are used
	// when running via `go run` in local development.
	Version   string
	BuildTime string
	GitCommit string
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
	ConnMaxIdleTime time.Duration
}

// DSN builds a PostgreSQL connection string from the database config.
func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.Name, d.SSLMode,
	)
}

// RedisConfig holds ElastiCache/Redis connection settings.
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
	// TLS must be enabled when talking to an ElastiCache cluster that has
	// in-transit encryption turned on.
	TLS bool
}

// JWTConfig holds signing secrets and token lifetimes.
type JWTConfig struct {
	AccessSecret    string
	RefreshSecret   string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	Issuer          string
}

// CORSConfig holds the cross-origin policy. Production deployments must
// enumerate origins explicitly; "*" is rejected by Validate.
type CORSConfig struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	MaxAge         time.Duration
}

// AllowsAll reports whether the policy is the permissive local default.
func (c CORSConfig) AllowsAll() bool {
	for _, o := range c.AllowedOrigins {
		if o == "*" {
			return true
		}
	}
	return false
}

// GraphQLConfig holds the GraphQL transport's protection limits.
type GraphQLConfig struct {
	// Path is where the GraphQL endpoint is mounted.
	Path string
	// MaxDepth is the deepest selection-set nesting a query may use.
	MaxDepth int
	// MaxComplexity is the highest static cost a query may carry, where
	// cost approximates the number of resolver invocations the query can
	// trigger (see graph/complexity.go).
	MaxComplexity int
	// MaxQueryBytes rejects oversized documents before parsing.
	MaxQueryBytes int
	// EnablePlayground serves an in-browser IDE at Path.
	EnablePlayground bool
	// EnableIntrospection allows __schema/__type queries. Disabled in
	// production so the schema is not self-publishing.
	EnableIntrospection bool
}

// ExportConfig configures the asynchronous usage-export pipeline.
type ExportConfig struct {
	// Backend selects the object store: "local" (filesystem, for local dev
	// and CI) or "s3".
	Backend string
	// LocalDir is where the local backend writes generated CSV objects.
	LocalDir string
	// S3Bucket is the private bucket generated CSVs are written to.
	S3Bucket string
	// S3Prefix namespaces objects inside the bucket.
	S3Prefix string
	// DownloadTTL is how long a generated download URL stays valid.
	DownloadTTL time.Duration
	// SigningKey signs local-backend download URLs. Falls back to the JWT
	// access secret when unset.
	SigningKey string
	// RunInProcessWorker starts the export worker inside the API process.
	// Convenient locally; in production the worker runs as its own ECS
	// service (cmd/worker) or as a Lambda consumer.
	RunInProcessWorker bool
	// WorkerPollInterval is how often the worker polls for queued jobs.
	WorkerPollInterval time.Duration
	// WorkerBatchSize is how many jobs one poll may claim.
	WorkerBatchSize int
	// MaxRows caps the number of usage rows a single export may contain.
	MaxRows int
	// MaxAttempts is how many times a failing job is retried before it is
	// parked as permanently failed (dead-letter equivalent).
	MaxAttempts int
	// SQSQueueURL, when set, mirrors job notifications onto SQS so a Lambda
	// consumer can process them instead of the polling worker.
	SQSQueueURL string
}

// AWSConfig holds shared AWS settings used by the cloud adapters.
type AWSConfig struct {
	Region string
	// Static credentials are only used for local emulation (MinIO,
	// LocalStack). On ECS, credentials come from the task role and these
	// stay empty.
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// S3Endpoint overrides the AWS endpoint, for MinIO/LocalStack.
	S3Endpoint string
	// SecretsManagerSecretID, when set, is loaded before environment
	// resolution so secrets never appear in the task definition.
	SecretsManagerSecretID string
}

// Load reads a .env file (if present) and then resolves configuration from
// the process environment, applying sane defaults for local development.
// In production the .env file will not exist and real environment
// variables (injected by ECS from Secrets Manager) are used instead.
func Load() (*Config, error) {
	_ = godotenv.Load() // ignore error: .env is optional (e.g. in containers)

	env := getEnv("APP_ENV", EnvDevelopment)
	isProd := env == EnvProduction

	cfg := &Config{
		Env:  env,
		Port: getEnv("APP_PORT", "8080"),

		DB: DatabaseConfig{
			Host:            getEnv("DB_HOST", "localhost"),
			Port:            getEnv("DB_PORT", "5432"),
			User:            getEnv("DB_USER", "bridgecore"),
			Password:        getEnv("DB_PASSWORD", "bridgecore"),
			Name:            getEnv("DB_NAME", "bridgecore"),
			SSLMode:         getEnv("DB_SSLMODE", defaultString(isProd, "require", "disable")),
			MaxOpenConns:    getEnvInt("DB_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    getEnvInt("DB_MAX_IDLE_CONNS", 10),
			ConnMaxLifetime: getEnvDuration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
			ConnMaxIdleTime: getEnvDuration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute),
		},

		Redis: RedisConfig{
			Addr:     getEnv("REDIS_ADDR", "localhost:6379"),
			Password: getEnv("REDIS_PASSWORD", ""),
			DB:       getEnvInt("REDIS_DB", 0),
			TLS:      getEnvBool("REDIS_TLS", false),
		},

		JWT: JWTConfig{
			AccessSecret:    getEnv("JWT_ACCESS_SECRET", defaultAccessSecret),
			RefreshSecret:   getEnv("JWT_REFRESH_SECRET", defaultRefreshSecret),
			AccessTokenTTL:  getEnvDuration("JWT_ACCESS_TTL", 15*time.Minute),
			RefreshTokenTTL: getEnvDuration("JWT_REFRESH_TTL", 7*24*time.Hour),
			Issuer:          getEnv("JWT_ISSUER", "bridgecore"),
		},

		CORS: CORSConfig{
			AllowedOrigins: getEnvList("CORS_ALLOWED_ORIGINS", []string{"*"}),
			AllowedMethods: getEnvList("CORS_ALLOWED_METHODS", []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}),
			AllowedHeaders: getEnvList("CORS_ALLOWED_HEADERS", []string{"Content-Type", "Authorization", "X-API-Key", "X-Request-ID"}),
			MaxAge:         getEnvDuration("CORS_MAX_AGE", 10*time.Minute),
		},

		GraphQL: GraphQLConfig{
			Path:                getEnv("GRAPHQL_PATH", "/graphql"),
			MaxDepth:            getEnvInt("GRAPHQL_MAX_DEPTH", 10),
			MaxComplexity:       getEnvInt("GRAPHQL_MAX_COMPLEXITY", 500),
			MaxQueryBytes:       getEnvInt("GRAPHQL_MAX_QUERY_BYTES", 16*1024),
			EnablePlayground:    getEnvBool("GRAPHQL_PLAYGROUND", !isProd),
			EnableIntrospection: getEnvBool("GRAPHQL_INTROSPECTION", !isProd),
		},

		Exports: ExportConfig{
			Backend:            getEnv("EXPORT_BACKEND", "local"),
			LocalDir:           getEnv("EXPORT_LOCAL_DIR", "./var/exports"),
			S3Bucket:           getEnv("EXPORT_S3_BUCKET", ""),
			S3Prefix:           getEnv("EXPORT_S3_PREFIX", "usage-exports"),
			DownloadTTL:        getEnvDuration("EXPORT_DOWNLOAD_TTL", 15*time.Minute),
			SigningKey:         getEnv("EXPORT_SIGNING_KEY", ""),
			RunInProcessWorker: getEnvBool("EXPORT_IN_PROCESS_WORKER", true),
			WorkerPollInterval: getEnvDuration("EXPORT_WORKER_POLL_INTERVAL", 2*time.Second),
			WorkerBatchSize:    getEnvInt("EXPORT_WORKER_BATCH_SIZE", 5),
			MaxRows:            getEnvInt("EXPORT_MAX_ROWS", 500000),
			MaxAttempts:        getEnvInt("EXPORT_MAX_ATTEMPTS", 3),
			SQSQueueURL:        getEnv("EXPORT_SQS_QUEUE_URL", ""),
		},

		AWS: AWSConfig{
			Region:                 getEnv("AWS_REGION", "ap-south-1"),
			AccessKeyID:            getEnv("AWS_ACCESS_KEY_ID", ""),
			SecretAccessKey:        getEnv("AWS_SECRET_ACCESS_KEY", ""),
			SessionToken:           getEnv("AWS_SESSION_TOKEN", ""),
			S3Endpoint:             getEnv("AWS_S3_ENDPOINT", ""),
			SecretsManagerSecretID: getEnv("AWS_SECRETS_MANAGER_SECRET_ID", ""),
		},

		APIKeyPrefix:       getEnv("API_KEY_PREFIX", "bc_live_"),
		PlatformAdminToken: getEnv("PLATFORM_ADMIN_TOKEN", ""),

		RateLimitRequestsPerMinute: getEnvInt("RATE_LIMIT_RPM", 120),

		MaxPageSize:     getEnvInt("MAX_PAGE_SIZE", 100),
		DefaultPageSize: getEnvInt("DEFAULT_PAGE_SIZE", 20),

		ExposeDevTools: getEnvBool("EXPOSE_DEV_TOOLS", !isProd),

		Version:   getEnv("APP_VERSION", "0.1.0-dev"),
		BuildTime: getEnv("APP_BUILD_TIME", "unknown"),
		GitCommit: getEnv("APP_GIT_COMMIT", "unknown"),
	}

	if cfg.Exports.SigningKey == "" {
		cfg.Exports.SigningKey = cfg.JWT.AccessSecret
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// IsProduction reports whether this process is configured as production.
func (c *Config) IsProduction() bool { return c.Env == EnvProduction }

// Validate enforces the invariants that must hold before the process serves
// traffic. Development gets working defaults; production gets a hard
// failure, because a misconfigured production deployment is a security
// incident rather than an inconvenience.
func (c *Config) Validate() error {
	var problems []string

	switch c.Env {
	case EnvDevelopment, EnvStaging, EnvProduction:
	default:
		problems = append(problems, fmt.Sprintf("APP_ENV must be one of development|staging|production, got %q", c.Env))
	}

	if c.Port == "" {
		problems = append(problems, "APP_PORT must not be empty")
	}
	if c.DB.Host == "" || c.DB.Name == "" || c.DB.User == "" {
		problems = append(problems, "DB_HOST, DB_NAME and DB_USER are required")
	}
	if c.DB.MaxIdleConns > c.DB.MaxOpenConns {
		problems = append(problems, "DB_MAX_IDLE_CONNS must not exceed DB_MAX_OPEN_CONNS")
	}
	if c.Redis.Addr == "" {
		problems = append(problems, "REDIS_ADDR is required")
	}
	if c.RateLimitRequestsPerMinute <= 0 {
		problems = append(problems, "RATE_LIMIT_RPM must be greater than 0")
	}
	if c.MaxPageSize <= 0 {
		problems = append(problems, "MAX_PAGE_SIZE must be greater than 0")
	}
	if c.DefaultPageSize <= 0 || c.DefaultPageSize > c.MaxPageSize {
		problems = append(problems, "DEFAULT_PAGE_SIZE must be in (0, MAX_PAGE_SIZE]")
	}
	if c.GraphQL.MaxDepth <= 0 {
		problems = append(problems, "GRAPHQL_MAX_DEPTH must be greater than 0")
	}
	if c.GraphQL.MaxComplexity <= 0 {
		problems = append(problems, "GRAPHQL_MAX_COMPLEXITY must be greater than 0")
	}
	if c.GraphQL.MaxQueryBytes <= 0 {
		problems = append(problems, "GRAPHQL_MAX_QUERY_BYTES must be greater than 0")
	}
	if !strings.HasPrefix(c.GraphQL.Path, "/") {
		problems = append(problems, "GRAPHQL_PATH must start with /")
	}
	if c.JWT.AccessTokenTTL <= 0 || c.JWT.RefreshTokenTTL <= 0 {
		problems = append(problems, "JWT TTLs must be positive durations")
	}
	if c.JWT.AccessTokenTTL >= c.JWT.RefreshTokenTTL {
		problems = append(problems, "JWT_ACCESS_TTL must be shorter than JWT_REFRESH_TTL")
	}
	if c.JWT.AccessSecret == c.JWT.RefreshSecret {
		problems = append(problems, "JWT_ACCESS_SECRET and JWT_REFRESH_SECRET must differ, so a refresh token can never be replayed as an access token")
	}
	if c.APIKeyPrefix == "" {
		problems = append(problems, "API_KEY_PREFIX must not be empty")
	}
	if c.PlatformAdminToken != "" && len(c.PlatformAdminToken) < 24 {
		problems = append(problems, "PLATFORM_ADMIN_TOKEN must be at least 24 characters when set")
	}

	switch c.Exports.Backend {
	case "local":
		if c.Exports.LocalDir == "" {
			problems = append(problems, "EXPORT_LOCAL_DIR is required when EXPORT_BACKEND=local")
		}
	case "s3":
		if c.Exports.S3Bucket == "" {
			problems = append(problems, "EXPORT_S3_BUCKET is required when EXPORT_BACKEND=s3")
		}
		if c.AWS.Region == "" {
			problems = append(problems, "AWS_REGION is required when EXPORT_BACKEND=s3")
		}
	default:
		problems = append(problems, fmt.Sprintf("EXPORT_BACKEND must be local|s3, got %q", c.Exports.Backend))
	}
	if c.Exports.MaxRows <= 0 {
		problems = append(problems, "EXPORT_MAX_ROWS must be greater than 0")
	}
	if c.Exports.MaxAttempts <= 0 {
		problems = append(problems, "EXPORT_MAX_ATTEMPTS must be greater than 0")
	}
	if c.Exports.DownloadTTL <= 0 {
		problems = append(problems, "EXPORT_DOWNLOAD_TTL must be a positive duration")
	}

	if c.IsProduction() {
		if c.JWT.AccessSecret == defaultAccessSecret || c.JWT.RefreshSecret == defaultRefreshSecret {
			problems = append(problems, "refusing to start in production with the default JWT secrets")
		}
		if len(c.JWT.AccessSecret) < minProductionSecretLength || len(c.JWT.RefreshSecret) < minProductionSecretLength {
			problems = append(problems, fmt.Sprintf("production JWT secrets must be at least %d characters", minProductionSecretLength))
		}
		if c.CORS.AllowsAll() {
			problems = append(problems, "refusing to start in production with wildcard CORS; set CORS_ALLOWED_ORIGINS explicitly")
		}
		if c.DB.SSLMode == "disable" {
			problems = append(problems, "refusing to start in production with DB_SSLMODE=disable")
		}
		if c.DB.Password == "bridgecore" || c.DB.Password == "" {
			problems = append(problems, "refusing to start in production with the default or empty database password")
		}
		if c.GraphQL.EnableIntrospection {
			problems = append(problems, "GraphQL introspection must be disabled in production (set GRAPHQL_INTROSPECTION=false)")
		}
		if c.GraphQL.EnablePlayground {
			problems = append(problems, "the GraphQL playground must be disabled in production (set GRAPHQL_PLAYGROUND=false)")
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("config: invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
}

// Redacted returns a copy-safe map of the configuration for start-up
// logging, with every credential removed. Logging configuration at boot is
// invaluable when debugging a deployment; logging secrets is how they leak
// into CloudWatch.
func (c *Config) Redacted() map[string]any {
	return map[string]any{
		"env":                  c.Env,
		"port":                 c.Port,
		"version":              c.Version,
		"git_commit":           c.GitCommit,
		"db_host":              c.DB.Host,
		"db_name":              c.DB.Name,
		"db_sslmode":           c.DB.SSLMode,
		"db_max_open_conns":    c.DB.MaxOpenConns,
		"redis_addr":           c.Redis.Addr,
		"redis_tls":            c.Redis.TLS,
		"cors_allowed_origins": c.CORS.AllowedOrigins,
		"rate_limit_rpm":       c.RateLimitRequestsPerMinute,
		"max_page_size":        c.MaxPageSize,
		"graphql_path":         c.GraphQL.Path,
		"graphql_max_depth":    c.GraphQL.MaxDepth,
		"graphql_complexity":   c.GraphQL.MaxComplexity,
		"graphql_playground":   c.GraphQL.EnablePlayground,
		"graphql_introspect":   c.GraphQL.EnableIntrospection,
		"export_backend":       c.Exports.Backend,
		"export_in_process":    c.Exports.RunInProcessWorker,
		"expose_dev_tools":     c.ExposeDevTools,
		"platform_api_enabled": c.PlatformAdminToken != "",
	}
}

// ErrSecretNotFound is returned by SecretLoader implementations when the
// requested secret does not exist.
var ErrSecretNotFound = errors.New("config: secret not found")

// SecretLoader resolves a bundle of secrets from an external provider
// (AWS Secrets Manager in production) as a flat key/value map. Keys are
// applied to the process environment before Load runs, so the rest of the
// configuration pipeline stays provider-agnostic.
type SecretLoader interface {
	LoadSecrets(secretID string) (map[string]string, error)
}

// ApplySecrets writes resolved secrets into the process environment without
// overwriting values that are already explicitly set, so an operator can
// always override a single value for a one-off debug session.
func ApplySecrets(secrets map[string]string) {
	for k, v := range secrets {
		if _, exists := os.LookupEnv(k); exists {
			continue
		}
		_ = os.Setenv(k, v)
	}
}

func defaultString(cond bool, whenTrue, whenFalse string) string {
	if cond {
		return whenTrue
	}
	return whenFalse
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

func getEnvBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
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

// getEnvList parses a comma-separated environment variable, trimming
// whitespace and dropping empty entries.
func getEnvList(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
