#!/usr/bin/env bash
# Docker confines Alpine/musl build dependencies; only artifacts leave it.
set -euo pipefail
cd "$(dirname "$0")/.."
arch="${1:-$(uname -m)}"
case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) exit 1 ;; esac
# Pin each platform manifest separately (also works with Docker's classic
# image store, which cannot retain two architectures under one index digest).
case "$arch" in
  amd64) image='alpine@sha256:85f2b723e106c34644cd5851d7e81ee87da98ac54672b29947c052a45d31dc2f' ;;
  arm64) image='alpine@sha256:24bb3511a0db7b5114a4aee033c65a8a4148f39b7b80a398e548546db967a36f' ;;
esac
docker run --rm --platform "linux/$arch" -e VERSION="${VERSION:-dev}" -e BUILD_JOBS="${BUILD_JOBS:-2}" \
  -e WHISPER_TARGET_OS=linux -e WHISPER_TARGET_ARCH="$arch" \
  -v "$PWD:/src" -w /src "$image" \
  sh -ec 'apk add --no-cache bash build-base cmake curl linux-headers; bash scripts/build-whisper.sh; chown -R '"$(id -u):$(id -g)"' dist/whisper'
