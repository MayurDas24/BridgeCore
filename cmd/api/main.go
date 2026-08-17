// Command api is the BridgeCore HTTP server entrypoint. It wires together
// configuration, logging, database/redis connections, repositories, services,
// the REST and GraphQL transports, and the export worker, then serves with
// graceful shutdown.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/graph"
	"github.com/bridgecore/bridgecore/internal/config"
	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/exports"
	"github.com/bridgecore/bridgecore/internal/handler"
	"github.com/bridgecore/bridgecore/internal/logger"
	mw "github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/jwt"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// exportDownloadPath is the route that serves signed downloads from the local
// object-store backend.
const exportDownloadPath = "/api/v1/usage/exports/download"

func main() {
	// A minimal logger exists before configuration, because secret loading and
	// configuration validation both need somewhere to report failures.
	bootLog, err := logger.New(config.EnvDevelopment)
	if err != nil {
		panic(err)
	}

	loadSecrets(bootLog)

	cfg, err := config.Load()
	if err != nil {
		// Configuration failures are fatal by design: a process that cannot be
		// configured safely must not start and serve traffic in a degraded
		// security posture.
		bootLog.Fatal("invalid configuration", zap.Error(err))
	}

	log, err := logger.New(cfg.Env)
	if err != nil {
		panic(err)
	}
	defer func() { _ = log.Sync() }()

	log.Info("starting BridgeCore API", zap.Any("config", cfg.Redacted()))

	// Correlation IDs are attached to every error envelope, so a user can quote
	// one and an operator can find the exact request in CloudWatch.
	response.RequestIDFunc = mw.RequestIDOf
	handler.ConfigurePagination(cfg.DefaultPageSize, cfg.MaxPageSize)

	// ---- Infrastructure ----
	db, err := connectWithRetry(cfg, log)
	if err != nil {
		log.Fatal("failed to connect to postgres", zap.Error(err))
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		log.Fatal("failed to run migrations", zap.Error(err))
	}
	log.Info("database migrations applied successfully")

	rdb, err := database.NewRedis(cfg.Redis)
	if err != nil {
		log.Fatal("failed to connect to redis", zap.Error(err))
	}
	defer rdb.Close()

	// ---- Repositories ----
	tenantRepo := repository.NewTenantRepository(db)
	userRepo := repository.NewUserRepository(db)
	refreshTokenRepo := repository.NewRefreshTokenRepository(db)
	entitlementRepo := repository.NewEntitlementRepository(db)
	apiKeyRepo := repository.NewAPIKeyRepository(db)
	usageRepo := repository.NewUsageRepository(db)
	auditRepo := repository.NewAuditRepository(db)
	exportRepo := repository.NewExportRepository(db)

	// ---- Export pipeline ----
	objectStore, localStore, err := objectStoreFor(cfg, exportDownloadPath)
	if err != nil {
		log.Fatal("failed to configure the export object store", zap.Error(err))
	}
	notifier := notifierFor(cfg, log)

	// ---- Services ----
	jwtManager := jwt.NewManager(
		cfg.JWT.AccessSecret, cfg.JWT.RefreshSecret,
		cfg.JWT.AccessTokenTTL, cfg.JWT.RefreshTokenTTL, cfg.JWT.Issuer,
	)
	auditSvc := service.NewAuditService(auditRepo, log)
	authSvc := service.NewAuthService(userRepo, tenantRepo, refreshTokenRepo, jwtManager)
	tenantSvc := service.NewTenantService(tenantRepo)
	userSvc := service.NewUserService(userRepo)
	entitlementSvc := service.NewEntitlementService(entitlementRepo)
	apiKeySvc := service.NewAPIKeyService(apiKeyRepo, cfg.APIKeyPrefix)
	usageSvc := service.NewUsageService(usageRepo)
	exportSvc := service.NewExportService(
		exportRepo, objectStore, notifier,
		cfg.Exports.DownloadTTL, cfg.Exports.MaxRows, log,
	)

	// ---- Handlers ----
	authHandler := handler.NewAuthHandler(authSvc, auditSvc)
	tenantHandler := handler.NewTenantHandler(tenantSvc, auditSvc)
	userHandler := handler.NewUserHandler(userSvc, auditSvc)
	entitlementHandler := handler.NewEntitlementHandler(entitlementSvc)
	apiKeyHandler := handler.NewAPIKeyHandler(apiKeySvc, auditSvc)
	usageHandler := handler.NewUsageHandler(usageSvc)
	exportHandler := handler.NewExportHandler(exportSvc, auditSvc, localStore)
	auditHandler := handler.NewAuditHandler(auditSvc)
	platformHandler := handler.NewPlatformHandler(tenantSvc, entitlementSvc, auditSvc)
	healthHandler := handler.NewHealthHandler(
		db, rdb, exportSvc, cfg.Version, cfg.BuildTime, cfg.GitCommit, cfg.Env,
	)

	// ---- GraphQL ----
	// The resolver receives services, never repositories: a resolver cannot
	// reach the database except through the same business logic REST uses.
	resolver := &graph.Resolver{
		Users:           userSvc,
		Tenants:         tenantSvc,
		Entitlements:    entitlementSvc,
		APIKeys:         apiKeySvc,
		Usage:           usageSvc,
		Audit:           auditSvc,
		Exports:         exportSvc,
		TenantSource:    tenantRepo,
		MaxPageSize:     cfg.MaxPageSize,
		DefaultPageSize: cfg.DefaultPageSize,
		Log:             log,
	}

	schema, err := graph.NewSchema(resolver)
	if err != nil {
		log.Fatal("failed to build the GraphQL schema", zap.Error(err))
	}

	graphqlHandler := graph.NewHandler(graph.HandlerConfig{
		Schema:       schema,
		TenantSource: tenantRepo,
		Limits: graph.Limits{
			MaxDepth:           cfg.GraphQL.MaxDepth,
			MaxComplexity:      cfg.GraphQL.MaxComplexity,
			MaxBytes:           cfg.GraphQL.MaxQueryBytes,
			MaxPageSize:        cfg.MaxPageSize,
			AllowIntrospection: cfg.GraphQL.EnableIntrospection,
		},
		Audit:             auditSvc,
		Log:               log,
		PlaygroundEnabled: cfg.GraphQL.EnablePlayground,
		Path:              cfg.GraphQL.Path,
	})

	// ---- Router ----
	mux := http.NewServeMux()
	registerRoutes(mux, routerDeps{
		cfg:                cfg,
		log:                log,
		jwtManager:         jwtManager,
		apiKeySvc:          apiKeySvc,
		tenantSvc:          tenantSvc,
		entitlementSvc:     entitlementSvc,
		auditSvc:           auditSvc,
		usageSvc:           usageSvc,
		redis:              rdb.Client,
		authHandler:        authHandler,
		tenantHandler:      tenantHandler,
		userHandler:        userHandler,
		entitlementHandler: entitlementHandler,
		apiKeyHandler:      apiKeyHandler,
		usageHandler:       usageHandler,
		exportHandler:      exportHandler,
		auditHandler:       auditHandler,
		platformHandler:    platformHandler,
		healthHandler:      healthHandler,
		graphqlHandler:     graphqlHandler,
	})

	// Global middleware applied to every route.
	globalChain := mw.Chain(
		mw.RequestID,
		mw.Recovery(log),
		mw.SecurityHeaders,
		mw.CORS(cfg.CORS),
		mw.Logging(log),
	)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: globalChain(mux),
		// ReadHeaderTimeout specifically defends against Slowloris: a client
		// that opens a connection and dribbles headers forever.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// ---- Background export worker ----
	// Running the worker in-process is the local and small-deployment mode. At
	// scale it runs as its own ECS service (cmd/worker) so export throughput
	// scales independently of API traffic, or as a Lambda consumer driven by
	// SQS. All three execute the same exports.Worker.
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()

	if cfg.Exports.RunInProcessWorker {
		worker := exports.NewWorker(exportRepo, usageRepo, objectStore, workerConfigFrom(cfg), log)
		go worker.Run(workerCtx)
	} else {
		log.Info("in-process export worker disabled; expecting a dedicated worker or Lambda consumer")
	}

	// ---- Serve with graceful shutdown ----
	go func() {
		log.Info("BridgeCore API listening",
			zap.String("port", cfg.Port),
			zap.String("graphql", cfg.GraphQL.Path),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("server failed", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutdown signal received, draining connections")

	// Stop accepting work before draining, so no new export is claimed while
	// the process is on its way out.
	stopWorker()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	} else {
		log.Info("server shut down cleanly")
	}
}

// connectWithRetry retries the initial Postgres connection, since in
// `docker compose up` (and during an ECS rolling deploy against a database
// that is still failing over) the API frequently starts before Postgres is
// accepting connections.
func connectWithRetry(cfg *config.Config, log *zap.Logger) (*database.DB, error) {
	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		db, err := database.NewPostgres(cfg.DB)
		if err == nil {
			return db, nil
		}
		lastErr = err
		log.Warn("postgres not ready yet, retrying",
			zap.Int("attempt", attempt), zap.Error(err))
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return nil, lastErr
}

func getenv(key string) string { return os.Getenv(key) }
