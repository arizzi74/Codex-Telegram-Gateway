#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
test "$(find dist -maxdepth 1 -name '*.tar.gz' -type f | wc -l)" -eq 10
(cd dist && sha256sum -c SHA256SUMS)
verify_stage=$(mktemp -d)
trap 'rm -rf -- "$verify_stage"' EXIT
for archive in dist/*.tar.gz; do
  artifact_stage="$verify_stage/$(basename "$archive" .tar.gz)"
  mkdir -p "$artifact_stage"
  tar -xzf "$archive" -C "$artifact_stage"
  (cd "$artifact_stage" && sha256sum -c SHA256SUMS)
done
