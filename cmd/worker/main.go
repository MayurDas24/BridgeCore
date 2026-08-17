// Command worker runs the BridgeCore export worker as a standalone process.
//
// It exists so export throughput can scale independently of API traffic: a
// tenant generating a 500,000-row export should not compete for CPU with
// request handling, and the number of workers should be able to change without
// redeploying the API. In production this runs as its own ECS service against
// the same database; the API then starts with EXPORT_IN_PROCESS_WORKER=false.
//
// It is the same exports.Worker the API embeds, so there is exactly one
// implementation of export generation, retries, and object storage.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/internal/cloud"
	"github.com/bridgecore/bridgecore/internal/config"
	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/exports"
	"github.com/bridgecore/bridgecore/internal/logger"
	"github.com/bridgecore/bridgecore/internal/repository"
)

func main() {
	bootLog, err := logger.New(config.EnvDevelopment)
	if err != nil {
		panic(err)
	}

	if secretID := os.Getenv("AWS_SECRETS_MANAGER_SECRET_ID"); secretID != "" {
		region := os.Getenv("AWS_REGION")
		creds := cloud.NewCredentialProvider(
			os.Getenv("AWS_ACCESS_KEY_ID"),
			os.Getenv("AWS_SECRET_ACCESS_KEY"),
			os.Getenv("AWS_SESSION_TOKEN"),
		)
		if secrets, err := cloud.NewSecretsManagerLoader(region, creds).LoadSecrets(secretID); err == nil {
			config.ApplySecrets(secrets)
		} else {
			bootLog.Error("failed to load worker secrets", zap.Error(err))
		}
	}

	cfg, err := config.Load()
	if err != nil {
		bootLog.Fatal("invalid configuration", zap.Error(err))
	}

	log, err := logger.New(cfg.Env)
	if err != nil {
		panic(err)
	}
	defer func() { _ = log.Sync() }()

	log = log.With(zap.String("component", "export-worker"))
	log.Info("starting BridgeCore export worker", zap.Any("config", cfg.Redacted()))

	db, err := database.NewPostgres(cfg.DB)
	if err != nil {
		log.Fatal("failed to connect to postgres", zap.Error(err))
	}
	defer db.Close()

	// The worker never runs migrations. Exactly one component owns schema
	// changes (the API, on boot); a second migrator racing it during a rolling
	// deploy is how a half-applied migration happens.

	store, _, err := objectStoreForWorker(cfg)
	if err != nil {
		log.Fatal("failed to configure the export object store", zap.Error(err))
	}

	worker := exports.NewWorker(
		repository.NewExportRepository(db),
		repository.NewUsageRepository(db),
		store,
		exports.WorkerConfig{
			PollInterval:      cfg.Exports.WorkerPollInterval,
			BatchSize:         cfg.Exports.WorkerBatchSize,
			MaxRows:           cfg.Exports.MaxRows,
			MaxAttempts:       cfg.Exports.MaxAttempts,
			ObjectPrefix:      cfg.Exports.S3Prefix,
			VisibilityTimeout: 10 * time.Minute,
		},
		log,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		log.Info("shutdown signal received; finishing the current job then stopping")
		cancel()
	}()

	worker.Run(ctx)
	log.Info("export worker stopped cleanly")
}

func objectStoreForWorker(cfg *config.Config) (exports.ObjectStore, *exports.LocalStore, error) {
	switch cfg.Exports.Backend {
	case "s3":
		creds := cloud.NewCredentialProvider(
			cfg.AWS.AccessKeyID, cfg.AWS.SecretAccessKey, cfg.AWS.SessionToken,
		)
		return cloud.NewS3Store(cfg.Exports.S3Bucket, cfg.AWS.Region, cfg.AWS.S3Endpoint, creds), nil, nil
	default:
		local, err := exports.NewLocalStore(
			cfg.Exports.LocalDir, "/api/v1/usage/exports/download", cfg.Exports.SigningKey,
		)
		if err != nil {
			return nil, nil, err
		}
		return local, local, nil
	}
}
