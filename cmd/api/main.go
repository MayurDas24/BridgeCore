// Command api is the BridgeCore HTTP server entrypoint. It wires together
// configuration, logging, database/redis connections, repositories,
// services, middleware, and routes, then serves with graceful shutdown.
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

	"github.com/bridgecore/bridgecore/internal/config"
	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/handler"
	"github.com/bridgecore/bridgecore/internal/logger"
	mw "github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/jwt"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}

	log, err := logger.New(cfg.Env)
	if err != nil {
		panic(err)
	}
	defer log.Sync()

	log.Info("starting BridgeCore API",
		zap.String("env", cfg.Env),
		zap.String("version", cfg.Version),
	)

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

	rdb, err := database.NewRedis(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
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

	// ---- Services ----
	jwtManager := jwt.NewManager(cfg.JWT.AccessSecret, cfg.JWT.RefreshSecret, cfg.JWT.AccessTokenTTL, cfg.JWT.RefreshTokenTTL, cfg.JWT.Issuer)
	auditSvc := service.NewAuditService(auditRepo, log)
	authSvc := service.NewAuthService(userRepo, tenantRepo, refreshTokenRepo, jwtManager)
	tenantSvc := service.NewTenantService(tenantRepo)
	entitlementSvc := service.NewEntitlementService(entitlementRepo)
	apiKeySvc := service.NewAPIKeyService(apiKeyRepo, cfg.APIKeyPrefix)
	usageSvc := service.NewUsageService(usageRepo)

	// ---- Handlers ----
	authHandler := handler.NewAuthHandler(authSvc, auditSvc)
	tenantHandler := handler.NewTenantHandler(tenantSvc, auditSvc)
	userHandler := handler.NewUserHandler(userRepo, auditSvc)
	entitlementHandler := handler.NewEntitlementHandler(entitlementSvc)
	apiKeyHandler := handler.NewAPIKeyHandler(apiKeySvc, auditSvc)
	usageHandler := handler.NewUsageHandler(usageSvc)
	auditHandler := handler.NewAuditHandler(auditSvc)
	healthHandler := handler.NewHealthHandler(db, rdb, cfg.Version, cfg.BuildTime)

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
		auditHandler:       auditHandler,
		healthHandler:      healthHandler,
	})

	// Global middleware chain applied to every route.
	globalChain := mw.Chain(
		mw.RequestID,
		mw.Recovery(log),
		mw.CORS,
		mw.Logging(log),
	)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      globalChain(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ---- Serve with graceful shutdown ----
	go func() {
		log.Info("BridgeCore API listening", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("server failed", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutdown signal received, draining connections")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	} else {
		log.Info("server shut down cleanly")
	}
}

// connectWithRetry retries the initial Postgres connection a few times,
// since in `docker compose up` the API container frequently starts before
// Postgres has finished initializing, even with a healthcheck-based
// `depends_on`.
func connectWithRetry(cfg *config.Config, log *zap.Logger) (*database.DB, error) {
	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		db, err := database.NewPostgres(cfg.DB)
		if err == nil {
			return db, nil
		}
		lastErr = err
		log.Warn("postgres not ready yet, retrying", zap.Int("attempt", attempt), zap.Error(err))
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return nil, lastErr
}
