// Command lambda-export is the serverless consumer of the export queue.
//
// It is an alternative to the long-running cmd/worker service: same
// exports.Worker, same job table, same object store, different execution
// model. Which one you run is an operational decision (see infra/terraform/
// lambda.tf); both are safe to run at once, because jobs are claimed with
// FOR UPDATE SKIP LOCKED and whichever consumer claims one owns it.
//
// It implements the AWS Lambda custom runtime protocol directly over the
// runtime API rather than using aws-lambda-go. The protocol is three HTTP
// calls — fetch an invocation, post a response, post an error — and
// implementing it with net/http keeps this project's dependency graph at six
// modules. The same reasoning applies here as to the hand-written SigV4
// signer in pkg/awssig: for a fixed, well-specified protocol, the SDK buys
// convenience rather than correctness.
//
// Build for Lambda (provided.al2023 expects a binary named "bootstrap"):
//
//	make lambda-build
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/internal/cloud"
	"github.com/bridgecore/bridgecore/internal/config"
	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/exports"
	"github.com/bridgecore/bridgecore/internal/logger"
	"github.com/bridgecore/bridgecore/internal/repository"
)

// sqsEvent is the subset of the SQS event source payload this consumer needs.
type sqsEvent struct {
	Records []struct {
		MessageID string `json:"messageId"`
		Body      string `json:"body"`
	} `json:"Records"`
}

// batchResponse reports partial batch failures, so a single bad message is
// retried (and eventually dead-lettered) without re-delivering the whole batch.
type batchResponse struct {
	BatchItemFailures []batchItemFailure `json:"batchItemFailures"`
}

type batchItemFailure struct {
	ItemIdentifier string `json:"itemIdentifier"`
}

func main() {
	log, err := logger.New(config.EnvProduction)
	if err != nil {
		panic(err)
	}
	defer func() { _ = log.Sync() }()

	log = log.With(zap.String("component", "export-lambda"))

	// Secrets are resolved once, during the cold start, and reused for every
	// warm invocation. Fetching them per invocation would add latency and
	// Secrets Manager calls to every single export.
	loadLambdaSecrets(log)

	cfg, err := config.Load()
	if err != nil {
		log.Fatal("invalid configuration", zap.Error(err))
	}

	// The database handle is created once and reused across warm invocations,
	// which is the whole reason this can safely share the API's RDS instance:
	// a connection per invocation would exhaust max_connections under any
	// real concurrency. Terraform caps reserved_concurrent_executions for the
	// same reason.
	db, err := database.NewPostgres(cfg.DB)
	if err != nil {
		log.Fatal("failed to connect to postgres", zap.Error(err))
	}
	defer db.Close()

	store, err := lambdaObjectStore(cfg)
	if err != nil {
		log.Fatal("failed to configure the export object store", zap.Error(err))
	}

	worker := exports.NewWorker(
		repository.NewExportRepository(db),
		repository.NewUsageRepository(db),
		store,
		exports.WorkerConfig{
			// The Lambda is invoked per notification, so it claims one job and
			// returns rather than polling. PollInterval is unused here.
			BatchSize:         1,
			MaxRows:           cfg.Exports.MaxRows,
			MaxAttempts:       cfg.Exports.MaxAttempts,
			ObjectPrefix:      cfg.Exports.S3Prefix,
			VisibilityTimeout: 10 * time.Minute,
		},
		log,
	)

	runtimeAPI := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	if runtimeAPI == "" {
		log.Fatal("AWS_LAMBDA_RUNTIME_API is not set; this binary only runs inside Lambda")
	}

	serve(runtimeAPI, worker, log)
}

// serve is the custom runtime loop: fetch an invocation, handle it, report the
// result, repeat until the execution environment is frozen or torn down.
func serve(runtimeAPI string, worker *exports.Worker, log *zap.Logger) {
	client := &http.Client{
		// No timeout: the next-invocation call is a long poll that blocks until
		// Lambda has work, which may be minutes. A timeout here would abort it.
		Timeout: 0,
	}
	base := "http://" + runtimeAPI + "/2018-06-01/runtime"

	for {
		event, requestID, err := nextInvocation(client, base)
		if err != nil {
			log.Error("failed to fetch the next invocation", zap.Error(err))
			time.Sleep(time.Second)
			continue
		}

		result, handleErr := handle(context.Background(), worker, event, log)
		if handleErr != nil {
			log.Error("invocation failed",
				zap.String("aws_request_id", requestID), zap.Error(handleErr))
			postError(client, base, requestID, handleErr)
			continue
		}

		postResponse(client, base, requestID, result)
	}
}

func nextInvocation(client *http.Client, base string) (sqsEvent, string, error) {
	resp, err := client.Get(base + "/invocation/next")
	if err != nil {
		return sqsEvent{}, "", err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	requestID := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")

	var event sqsEvent
	if err := json.NewDecoder(resp.Body).Decode(&event); err != nil {
		return sqsEvent{}, requestID, fmt.Errorf("decode invocation payload: %w", err)
	}
	return event, requestID, nil
}

// handle processes one SQS batch.
//
// The message body is only a pointer to a job row; the row is the source of
// truth. So the handler does not trust the message contents beyond using it as
// a wake-up signal — it claims whatever work the database says is pending. That
// makes a duplicate delivery harmless: the second one finds nothing to claim.
func handle(ctx context.Context, worker *exports.Worker, event sqsEvent, log *zap.Logger) (batchResponse, error) {
	var failures []batchItemFailure

	for _, record := range event.Records {
		var notification exports.JobNotification
		if err := json.Unmarshal([]byte(record.Body), &notification); err != nil {
			// A malformed message can never succeed, so failing it here sends
			// it to the DLQ immediately rather than retrying three times.
			log.Warn("discarding an unparsable queue message",
				zap.String("message_id", record.MessageID), zap.Error(err))
			continue
		}

		log.Info("export notification received",
			zap.String("export_job_id", notification.JobID),
			zap.String("tenant_id", notification.TenantID))

		processed, err := worker.RunOnce(ctx)
		if err != nil {
			log.Error("export processing failed",
				zap.String("export_job_id", notification.JobID), zap.Error(err))
			failures = append(failures, batchItemFailure{ItemIdentifier: record.MessageID})
			continue
		}

		log.Info("export batch complete",
			zap.String("export_job_id", notification.JobID),
			zap.Int("jobs_processed", processed))
	}

	return batchResponse{BatchItemFailures: failures}, nil
}

func postResponse(client *http.Client, base, requestID string, result batchResponse) {
	payload, err := json.Marshal(result)
	if err != nil {
		payload = []byte(`{"batchItemFailures":[]}`)
	}
	resp, err := client.Post(base+"/invocation/"+requestID+"/response",
		"application/json", bytes.NewReader(payload))
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func postError(client *http.Client, base, requestID string, cause error) {
	payload, _ := json.Marshal(map[string]string{
		"errorMessage": cause.Error(),
		"errorType":    "ExportProcessingError",
	})
	resp, err := client.Post(base+"/invocation/"+requestID+"/error",
		"application/json", bytes.NewReader(payload))
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// loadLambdaSecrets resolves both the application secret bundle and the
// RDS-managed master credential, since the Lambda needs the database password
// that the ECS agent would otherwise have injected.
func loadLambdaSecrets(log *zap.Logger) {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		return
	}

	creds := cloud.NewCredentialProvider(
		os.Getenv("AWS_ACCESS_KEY_ID"),
		os.Getenv("AWS_SECRET_ACCESS_KEY"),
		os.Getenv("AWS_SESSION_TOKEN"),
	)
	loader := cloud.NewSecretsManagerLoader(region, creds)

	if id := os.Getenv("AWS_SECRETS_MANAGER_SECRET_ID"); id != "" {
		if secrets, err := loader.LoadSecrets(id); err == nil {
			config.ApplySecrets(secrets)
		} else {
			log.Error("failed to load the application secret bundle", zap.Error(err))
		}
	}

	// The RDS-managed secret uses AWS's own key names, so it is mapped onto the
	// application's DB_PASSWORD rather than applied directly.
	if id := os.Getenv("RDS_SECRET_ID"); id != "" {
		if secrets, err := loader.LoadSecrets(id); err == nil {
			if password, ok := secrets["password"]; ok {
				config.ApplySecrets(map[string]string{"DB_PASSWORD": password})
			}
		} else {
			log.Error("failed to load the database credential", zap.Error(err))
		}
	}
}

func lambdaObjectStore(cfg *config.Config) (exports.ObjectStore, error) {
	creds := cloud.NewCredentialProvider(
		cfg.AWS.AccessKeyID, cfg.AWS.SecretAccessKey, cfg.AWS.SessionToken,
	)
	return cloud.NewS3Store(cfg.Exports.S3Bucket, cfg.AWS.Region, cfg.AWS.S3Endpoint, creds), nil
}
