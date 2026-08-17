package main

import (
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/internal/cloud"
	"github.com/bridgecore/bridgecore/internal/config"
	"github.com/bridgecore/bridgecore/internal/exports"
)

// loadSecrets pulls configuration secrets from AWS Secrets Manager before the
// environment is read, when a secret ID is configured.
//
// This runs before config.Load so the rest of the process is unaware of where
// its secrets came from: the ECS task definition carries only the secret's ARN
// and a task role permitted to read it, never a password. A leaked task
// definition, image layer, or Terraform state file therefore leaks nothing.
func loadSecrets(bootLog *zap.Logger) {
	secretID := getenv("AWS_SECRETS_MANAGER_SECRET_ID")
	if secretID == "" {
		return
	}

	region := getenv("AWS_REGION")
	if region == "" {
		bootLog.Warn("AWS_SECRETS_MANAGER_SECRET_ID is set but AWS_REGION is not; skipping secret load")
		return
	}

	creds := cloud.NewCredentialProvider(
		getenv("AWS_ACCESS_KEY_ID"),
		getenv("AWS_SECRET_ACCESS_KEY"),
		getenv("AWS_SESSION_TOKEN"),
	)

	secrets, err := cloud.NewSecretsManagerLoader(region, creds).LoadSecrets(secretID)
	if err != nil {
		// Deliberately fatal in effect: config.Validate will refuse to start
		// production without real secrets, so failing here produces a clear
		// message instead of a confusing validation error.
		bootLog.Error("failed to load secrets from AWS Secrets Manager", zap.Error(err))
		return
	}

	config.ApplySecrets(secrets)
	bootLog.Info("loaded configuration from AWS Secrets Manager", zap.Int("keys", len(secrets)))
}

// objectStoreFor builds the export object store for the configured backend.
//
// The returned *exports.LocalStore is non-nil only for the local backend, in
// which case the API also serves signed downloads itself. With S3 the client
// fetches the presigned URL directly and export bytes never pass through the
// API at all.
func objectStoreFor(cfg *config.Config, downloadPath string) (exports.ObjectStore, *exports.LocalStore, error) {
	switch cfg.Exports.Backend {
	case "local":
		local, err := exports.NewLocalStore(cfg.Exports.LocalDir, downloadPath, cfg.Exports.SigningKey)
		if err != nil {
			return nil, nil, err
		}
		return local, local, nil

	case "s3":
		creds := cloud.NewCredentialProvider(
			cfg.AWS.AccessKeyID, cfg.AWS.SecretAccessKey, cfg.AWS.SessionToken,
		)
		store := cloud.NewS3Store(cfg.Exports.S3Bucket, cfg.AWS.Region, cfg.AWS.S3Endpoint, creds)
		return store, nil, nil

	default:
		return nil, nil, fmt.Errorf("unsupported EXPORT_BACKEND %q", cfg.Exports.Backend)
	}
}

// notifierFor builds the job notifier. Without a queue URL the worker polls
// the job table, which is a perfectly valid production topology for a single
// worker service; with one, a Lambda consumer is woken immediately.
func notifierFor(cfg *config.Config, log *zap.Logger) exports.Notifier {
	if cfg.Exports.SQSQueueURL == "" {
		return exports.NoopNotifier{}
	}

	creds := cloud.NewCredentialProvider(
		cfg.AWS.AccessKeyID, cfg.AWS.SecretAccessKey, cfg.AWS.SessionToken,
	)
	notifier, err := cloud.NewSQSNotifier(cfg.Exports.SQSQueueURL, cfg.AWS.Region, creds)
	if err != nil {
		log.Error("failed to configure the SQS notifier; falling back to worker polling", zap.Error(err))
		return exports.NoopNotifier{}
	}
	return notifier
}

// workerConfigFrom maps application configuration onto the worker's tuning.
func workerConfigFrom(cfg *config.Config) exports.WorkerConfig {
	return exports.WorkerConfig{
		PollInterval:      cfg.Exports.WorkerPollInterval,
		BatchSize:         cfg.Exports.WorkerBatchSize,
		MaxRows:           cfg.Exports.MaxRows,
		MaxAttempts:       cfg.Exports.MaxAttempts,
		ObjectPrefix:      cfg.Exports.S3Prefix,
		VisibilityTimeout: 10 * time.Minute,
	}
}
