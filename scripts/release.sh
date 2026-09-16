#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version="${VERSION:-dev}"
rm -rf -- dist
mkdir -p dist
if command -v sha256sum >/dev/null 2>&1; then
  checksum() { sha256sum "$@"; }
else
  checksum() { shasum -a 256 "$@"; }
fi
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  target_os="${target%/*}"
  target_arch="${target#*/}"
  for binary in codex-gateway codex-worker codex-local; do
    if [[ "$binary" == codex-gateway && "$target_os" != linux ]]; then continue; fi
    artifact="${binary}-${target_os}-${target_arch}"
    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath -ldflags "-s -w -X github.com/iaia/telegramgw/internal/buildinfo.Version=${version}" -o "dist/${artifact}" "./cmd/${binary}"
    package_dir=$(mktemp -d)
    cp "dist/${artifact}" "${package_dir}/${binary}"
    cp README.md LICENSE "${package_dir}/"
    cp -R examples deploy migrations "${package_dir}/"
    mkdir -p "${package_dir}/scripts"
    cp scripts/migrate-postgres-to-sqlite.py scripts/release-manager.py scripts/install-worker.sh "${package_dir}/scripts/"
    (cd "$package_dir" && checksum "$binary" scripts/release-manager.py scripts/install-worker.sh > SHA256SUMS)
    tar -czf "dist/${artifact}.tar.gz" -C "$package_dir" .
    # mktemp creates a dedicated temporary staging directory owned by this script.
    rm -rf -- "$package_dir"
  done
done
cp scripts/release-manager.py dist/codex-telegramgw-manager.py
cp scripts/install.sh dist/install.sh
(cd dist && checksum ./*.tar.gz codex-telegramgw-manager.py install.sh > SHA256SUMS)
