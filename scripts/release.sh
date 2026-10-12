#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version="${VERSION:-dev}"
build_dir=$(mktemp -d)
trap 'rm -rf -- "$build_dir"' EXIT
# Native helpers are built on each target host by the native build matrix.
# Never silently omit a platform or substitute a Go stub in public releases.
whisper_dist="${WHISPER_DIST_DIR:-.native-dist}"
speech_dist="${SPEECH_DIST_DIR:-.speech-dist}"
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  cp "$whisper_dist/codex-whisper-${target%/*}-${target#*/}" "$build_dir/"
  for helper in codex-piper codex-qwen-tts; do
    cp "$speech_dist/$helper-${target%/*}-${target#*/}.tar.gz" "$build_dir/"
  done
done
# Only this dedicated build-output directory is replaced. Stage inputs first so
# local builds in dist/whisper remain usable as WHISPER_DIST_DIR.
rm -rf -- dist
mkdir -p dist
cp "$build_dir"/codex-whisper-* dist/
cp "$build_dir"/*.tar.gz dist/
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
  "$build_dir/release-tool" package --root . --dist dist --binary codex-whisper --os "$target_os" --arch "$target_arch"
done
cp scripts/install.sh dist/install.sh
"$build_dir/release-tool" manifest --dist dist
