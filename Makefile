.PHONY: help deps run run-worker build lambda-build test test-race test-cover integration vet fmt fmt-check lint seed \
        up down logs rebuild reset sync-migrations clean tf-init tf-plan tf-apply tf-fmt ci

APP_NAME    := bridgecore-api
WORKER_NAME := bridgecore-worker
SEED_NAME   := bridgecore-seed
BIN_DIR     := bin

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

## help: List the available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'

## deps: Download and tidy Go modules (run this first, after cloning)
deps:
	go mod tidy
	go mod verify

## run: Run the API locally (expects Postgres/Redis reachable per .env)
run:
	go run ./cmd/api

## run-worker: Run the export worker as a separate process
run-worker:
	go run ./cmd/worker

## build: Compile the API, worker and seed binaries into ./bin
build:
	mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/$(APP_NAME)    ./cmd/api
	go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/$(WORKER_NAME) ./cmd/worker
	go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/$(SEED_NAME)   ./cmd/seed

## lambda-build: Build the export Lambda deployment package into ./bin
lambda-build:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/bootstrap ./cmd/lambda-export
	cd $(BIN_DIR) && zip -q -FS lambda-export.zip bootstrap
	@echo "built $(BIN_DIR)/lambda-export.zip"

## test: Run the unit test suite
test:
	go test ./...

## test-race: Run tests under the race detector (required in CI)
test-race:
	go test -race ./...

## test-cover: Run tests with a coverage report
test-cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -20

## integration: Run integration tests against real Postgres and Redis
## Requires `make up` (or exported DB_HOST/REDIS_ADDR) first.
integration:
	go test -tags=integration -count=1 -v ./integration/...

## vet: Run go vet static analysis
vet:
	go vet ./...

## fmt: Format all Go source files
fmt:
	gofmt -s -l -w .

## fmt-check: Fail if any file is not gofmt-clean (what CI runs)
fmt-check:
	@unformatted=$$(gofmt -s -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt (run 'make fmt'):"; echo "$$unformatted"; exit 1; \
	fi

## lint: fmt-check + vet + build, the same gate CI applies
lint: fmt-check vet
	go build ./...

## ci: Everything CI runs, locally
ci: deps lint test-race

## seed: Populate baseline demo data (idempotent)
seed:
	go run ./cmd/seed

## up: Build and start the full stack (API + worker + Postgres + Redis), then seed
up:
	docker compose up --build -d postgres redis api worker
	docker compose run --rm seed

## down: Stop and remove the stack (keeps volumes)
down:
	docker compose down

## reset: Stop the stack and delete its volumes (wipes the database)
reset:
	docker compose down -v

## logs: Tail API and worker logs
logs:
	docker compose logs -f api worker

## rebuild: Rebuild images from scratch, no cache
rebuild:
	docker compose build --no-cache

## sync-migrations: Copy /migrations into the embedded copy the binary ships
## Run this after editing anything in /migrations.
sync-migrations:
	cp migrations/*.sql internal/database/migrations/

## tf-init: Initialise Terraform
tf-init:
	cd infra/terraform && terraform init

## tf-plan: Show the planned AWS infrastructure changes
tf-plan:
	cd infra/terraform && terraform plan

## tf-apply: Apply the AWS infrastructure
tf-apply:
	cd infra/terraform && terraform apply

## tf-fmt: Format Terraform files
tf-fmt:
	cd infra/terraform && terraform fmt -recursive

## clean: Remove build artifacts and local export scratch space
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html var/
