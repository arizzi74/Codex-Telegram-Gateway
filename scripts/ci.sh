#!/usr/bin/env bash
# Every integration test uses an isolated SQLite file; no database service is required.
set -euo pipefail
cd "$(dirname "$0")/.."
test -z "$(gofmt -l cmd internal migrations)"
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
# Python is only used to test the source-only PostgreSQL migration utilities.
# Installation, updates and release packaging use native Go executables.
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_sqlite_*.py'
# Allow the isolated SQLite integration suite to finish under race instrumentation.
go test -race ./... -timeout=600s
./scripts/release.sh
./scripts/verify-release.sh
