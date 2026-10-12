#!/usr/bin/env bash
# Static musl builds for either Linux release architecture.
set -euo pipefail
cd "$(dirname "$0")/.."
case "${1:-$(uname -m)}" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) exit 1 ;; esac
case "$arch" in
  amd64) image='alpine@sha256:85f2b723e106c34644cd5851d7e81ee87da98ac54672b29947c052a45d31dc2f' ;;
  arm64) image='alpine@sha256:24bb3511a0db7b5114a4aee033c65a8a4148f39b7b80a398e548546db967a36f' ;;
esac
mounts=(-v "$PWD:/src")
environment=(-e VERSION="${VERSION:-dev}" -e BUILD_JOBS="${BUILD_JOBS:-2}" -e QWEN_TTS_OUTPUT_DIR="${QWEN_TTS_OUTPUT_DIR:-dist/qwen-tts}")
if [[ -n "${QWEN_TTS_SOURCE_ARCHIVE:-}" ]]; then
  mounts+=(-v "$QWEN_TTS_SOURCE_ARCHIVE:/source.tar.gz:ro")
  environment+=(-e QWEN_TTS_SOURCE_ARCHIVE=/source.tar.gz)
fi
if [[ -n "${QWEN_TTS_MODEL_DIR:-}" ]]; then
  mounts+=(-v "$QWEN_TTS_MODEL_DIR:/models:ro")
  environment+=(-e QWEN_TTS_MODEL_DIR=/models)
fi
docker run --rm --platform "linux/$arch" "${environment[@]}" -e QWEN_TTS_TARGET_OS=linux -e QWEN_TTS_TARGET_ARCH="$arch" \
  "${mounts[@]}" -w /src "$image" \
  sh -ec 'apk add --no-cache bash build-base cmake curl linux-headers patch python3 tar; bash scripts/build-qwen-tts.sh; chown -R '"$(id -u):$(id -g)"' "${QWEN_TTS_OUTPUT_DIR:-dist/qwen-tts}"'
