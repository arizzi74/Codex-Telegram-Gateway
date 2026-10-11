#!/usr/bin/env bash
# Build the pinned CPU-only helper on the target host. Linux must use musl.
set -euo pipefail
cd "$(dirname "$0")/.."
source native/whisper/source.env
version="${VERSION:-dev}"
target_os="${WHISPER_TARGET_OS:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
case "${WHISPER_TARGET_ARCH:-$(uname -m)}" in x86_64|amd64) target_arch=amd64 ;; aarch64|arm64) target_arch=arm64 ;; *) exit 1 ;; esac
case "$target_os/$target_arch" in linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) ;; *) exit 1 ;; esac
output="${WHISPER_OUTPUT_DIR:-dist/whisper}"
mkdir -p "$output"
output=$(cd "$output" && pwd)
build_dir=$(mktemp -d)
trap 'rm -rf -- "$build_dir"' EXIT
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 3 \
  "https://codeload.github.com/ggml-org/whisper.cpp/tar.gz/$WHISPER_COMMIT" -o "$build_dir/source.tar.gz"
if command -v sha256sum >/dev/null; then source_hash=$(sha256sum "$build_dir/source.tar.gz" | cut -d ' ' -f 1)
else source_hash=$(shasum -a 256 "$build_dir/source.tar.gz" | cut -d ' ' -f 1); fi
[[ "$source_hash" == "$WHISPER_SOURCE_SHA256" ]] || { echo 'Whisper source checksum mismatch' >&2; exit 1; }
tar -xzf "$build_dir/source.tar.gz" -C "$build_dir"
cmake_args=(-DCMAKE_BUILD_TYPE=Release "-DCODEX_WHISPER_VERSION=$version" "-DWHISPER_SOURCE_DIR=$build_dir/whisper.cpp-$WHISPER_COMMIT")
if [[ "$target_os" == linux ]]; then
  "${CC:-cc}" -dumpmachine | grep -q musl || { echo 'Linux releases require a musl compiler; use build-whisper-linux.sh.' >&2; exit 1; }
else
  cmake_args+=(-DCMAKE_OSX_DEPLOYMENT_TARGET=13.0)
fi
cmake -S native/whisper -B "$build_dir/build" "${cmake_args[@]}"
cmake --build "$build_dir/build" --parallel "${BUILD_JOBS:-2}" --target codex-whisper protocol-test
ctest --test-dir "$build_dir/build" --output-on-failure
cp "$build_dir/build/codex-whisper" "$output/codex-whisper-$target_os-$target_arch"
chmod 755 "$output/codex-whisper-$target_os-$target_arch"
"$output/codex-whisper-$target_os-$target_arch" version
