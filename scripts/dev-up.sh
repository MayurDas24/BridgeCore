#!/usr/bin/env bash
# scripts/dev-up.sh
#
# Convenience script for local development without Docker: starts (or
# assumes already-running) local Postgres/Redis, applies .env, runs
# migrations via the API's own self-migration on boot, then seeds baseline
# data. Intended for contributors who prefer a native toolchain over
# Docker Compose during iteration.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ ! -f .env ]; then
  echo "No .env found — copying .env.example. Review it before continuing."
  cp .env.example .env
fi

echo "==> Building binaries"
mkdir -p bin
go build -trimpath -o bin/bridgecore-api ./cmd/api
go build -trimpath -o bin/bridgecore-seed ./cmd/seed

echo "==> Starting API in the background (logs: /tmp/bridgecore-api.log)"
set -a; source .env; set +a
./bin/bridgecore-api > /tmp/bridgecore-api.log 2>&1 &
API_PID=$!
echo "$API_PID" > /tmp/bridgecore-api.pid

echo "==> Waiting for the API to become healthy"
for i in $(seq 1 20); do
  if curl -sf http://localhost:"${APP_PORT:-8080}"/live > /dev/null 2>&1; then
    echo "API is up (pid $API_PID)."
    break
  fi
  sleep 1
done

echo "==> Seeding baseline data"
./bin/bridgecore-seed

echo ""
echo "Done. API listening on :${APP_PORT:-8080}. Stop it with:"
echo "  kill \$(cat /tmp/bridgecore-api.pid)"
