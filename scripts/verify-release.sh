#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
CGO_ENABLED=0 go run ./cmd/release-tool verify --root . --dist dist
