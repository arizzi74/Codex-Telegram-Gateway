#!/usr/bin/env bash
# The same job can run locally or on a CI runner with an isolated test database.
set -euo pipefail
cd "$(dirname "$0")/.."
: "${TEST_DATABASE_URL:?set TEST_DATABASE_URL to a PostgreSQL test database}"
test -z "$(gofmt -l cmd internal migrations)"
go vet ./...
go test -race ./... -timeout=90s
./scripts/release.sh
./scripts/verify-release.sh
