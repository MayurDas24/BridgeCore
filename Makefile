.PHONY: run build test test-verbose test-coverage vet lint fmt seed docker-up docker-down docker-logs docker-rebuild clean sync-migrations

APP_NAME := bridgecore-api
SEED_NAME := bridgecore-seed
BIN_DIR := bin

## run: Run the API locally with `go run` (expects local Postgres/Redis or .env pointing at them)
run:
	go run ./cmd/api

## build: Compile the API and seed binaries into ./bin
build:
	mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/$(APP_NAME) ./cmd/api
	go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/$(SEED_NAME) ./cmd/seed

## test: Run the unit test suite
test:
	go test ./...

## test-verbose: Run tests with verbose per-test output
test-verbose:
	go test -v ./...

## test-coverage: Run tests with coverage report
test-coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

## vet: Run go vet static analysis
vet:
	go vet ./...

## fmt: Format all Go source files
fmt:
	gofmt -l -w .

## seed: Run the seed command against the currently configured database
seed:
	go run ./cmd/seed

## docker-up: Build and start the full stack (API + Postgres + Redis), then seed
docker-up:
	docker compose up --build -d
	docker compose run --rm seed

## docker-down: Stop and remove the stack (keeps the Postgres volume)
docker-down:
	docker compose down

## docker-logs: Tail API logs
docker-logs:
	docker compose logs -f api

## docker-rebuild: Rebuild images from scratch, no cache
docker-rebuild:
	docker compose build --no-cache

## sync-migrations: Copy root /migrations into internal/database/migrations
## (the embedded copy used at compile time). Run this after editing a
## migration file in the root /migrations directory.
sync-migrations:
	cp migrations/*.sql internal/database/migrations/

## clean: Remove build artifacts
clean:
	rm -rf $(BIN_DIR) coverage.out
