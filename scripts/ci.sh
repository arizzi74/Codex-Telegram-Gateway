#!/usr/bin/env bash
# Every integration test uses an isolated SQLite file; no database service is required.
set -euo pipefail
cd "$(dirname "$0")/.."
test -z "$(gofmt -l cmd internal migrations)"
go vet ./...
python3 -m unittest discover -s scripts -p 'test_*.py'
go test -race ./... -timeout=90s
./scripts/release.sh
./scripts/verify-release.sh
