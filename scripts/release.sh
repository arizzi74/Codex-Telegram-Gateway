#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version="${VERSION:-dev}"
# Only this dedicated build-output directory is replaced.
rm -rf -- dist
mkdir -p dist
build_dir=$(mktemp -d)
trap 'rm -rf -- "$build_dir"' EXIT
CGO_ENABLED=0 go build -trimpath -o "$build_dir/release-tool" ./cmd/release-tool
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  target_os="${target%/*}"
  target_arch="${target#*/}"
  manager="codex-telegramgw-${target_os}-${target_arch}"
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath -ldflags "-s -w -X github.com/iaia/telegramgw/internal/buildinfo.Version=${version}" -o "dist/${manager}" ./cmd/codex-telegramgw
  for binary in codex-gateway codex-worker codex-local; do
    if [[ "$binary" == codex-gateway && "$target_os" != linux ]]; then continue; fi
    artifact="${binary}-${target_os}-${target_arch}"
    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath -ldflags "-s -w -X github.com/iaia/telegramgw/internal/buildinfo.Version=${version}" -o "dist/${artifact}" "./cmd/${binary}"
    "$build_dir/release-tool" package --root . --dist dist --binary "$binary" --os "$target_os" --arch "$target_arch"
  done
done
cp scripts/install.sh dist/install.sh
"$build_dir/release-tool" manifest --dist dist
