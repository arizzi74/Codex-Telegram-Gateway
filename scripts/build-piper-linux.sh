#!/usr/bin/env bash
# Ubuntu 22.04 confines the compiler baseline; no host ONNX/eSpeak packages.
set -euo pipefail
cd "$(dirname "$0")/.."
case "${1:-$(uname -m)}" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) exit 1 ;; esac
case "$arch" in
  amd64) image='ubuntu@sha256:08ea48a03a3e78ebc7cd526e6a275053223aadd88bfc09cc49b06d5281525fde' ;;
  arm64) image='ubuntu@sha256:30dfd7fe96f97c5f5ddf388ce8d098aa509644ad68fdef67244fb71b8169ffcb' ;;
esac
docker run --rm --platform "linux/$arch" -e VERSION="${VERSION:-dev}" -e BUILD_JOBS="${BUILD_JOBS:-2}" \
  -v "$PWD:/src" -w /src "$image" \
  bash -ec 'export DEBIAN_FRONTEND=noninteractive; apt-get update -qq; apt-get install -y --no-install-recommends build-essential cmake curl ca-certificates bzip2; bash scripts/build-piper.sh; chown -R '"$(id -u):$(id -g)"' dist/piper'
