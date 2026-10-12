#!/usr/bin/env bash
# Build the pinned CPU resident helper. Linux release builds use musl.
set -euo pipefail
cd "$(dirname "$0")/.."
source native/qwen-tts/source.env
version="${VERSION:-dev}"
target_os="${QWEN_TTS_TARGET_OS:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
case "${QWEN_TTS_TARGET_ARCH:-$(uname -m)}" in x86_64|amd64) target_arch=amd64 ;; aarch64|arm64) target_arch=arm64 ;; *) exit 1 ;; esac
case "$target_os/$target_arch" in linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) ;; *) exit 1 ;; esac
output="${QWEN_TTS_OUTPUT_DIR:-dist/qwen-tts}"
mkdir -p "$output"
output=$(cd "$output" && pwd)
build_dir=$(mktemp -d)
trap 'rm -rf -- "$build_dir"' EXIT
hash_file() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d ' ' -f 1
  else shasum -a 256 "$1" | cut -d ' ' -f 1; fi
}
if [[ -n "${QWEN_TTS_SOURCE_ARCHIVE:-}" ]]; then
  cp "$QWEN_TTS_SOURCE_ARCHIVE" "$build_dir/source.tar.gz"
else
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 3 \
    "https://api.github.com/repos/ggml-org/llama.cpp/tarball/$QWEN_TTS_COMMIT" -o "$build_dir/source.tar.gz"
fi
[[ "$(hash_file "$build_dir/source.tar.gz")" == "$QWEN_TTS_SOURCE_SHA256" ]] || { echo 'Qwen runtime source checksum mismatch' >&2; exit 1; }
[[ "$(hash_file native/qwen-tts/stock-giovanni.wav)" == "$QWEN_TTS_STOCK_VOICE_SHA256" ]] || { echo 'Qwen stock voice checksum mismatch' >&2; exit 1; }
mkdir "$build_dir/source"
tar -xzf "$build_dir/source.tar.gz" --strip-components=1 -C "$build_dir/source"
patch --batch --forward -d "$build_dir/source" -p1 < native/qwen-tts/threads.patch
cmake_args=(-DCMAKE_BUILD_TYPE=Release "-DCODEX_QWEN_TTS_VERSION=$version" "-DQWEN_TTS_SOURCE_DIR=$build_dir/source")
if [[ "$target_os" == linux ]]; then
  [[ "$(${CC:-cc} -dumpmachine)" == *musl* ]] || { echo 'Linux releases require a musl compiler; use build-qwen-tts-linux.sh.' >&2; exit 1; }
else
  cmake_args+=(-DCMAKE_OSX_DEPLOYMENT_TARGET=13.0 "-DCMAKE_OSX_ARCHITECTURES=$( [[ "$target_arch" == amd64 ]] && echo x86_64 || echo arm64 )")
fi
stage="$output/$target_os-$target_arch"
rm -rf -- "$stage"
mkdir -p "$stage/licenses" "$stage/lib"
for variant in baseline optimized; do
  optimized=OFF
  program=codex-qwen-tts
  if [[ "$variant" == optimized ]]; then
    optimized=ON
    program=lib/codex-qwen-tts-optimized
  fi
  cmake -S native/qwen-tts -B "$build_dir/build-$variant" "${cmake_args[@]}" "-DQWEN_TTS_OPTIMIZED=$optimized"
  cmake --build "$build_dir/build-$variant" --parallel "${BUILD_JOBS:-2}" --target codex-qwen-tts qwen-protocol-test
  ctest --test-dir "$build_dir/build-$variant" --output-on-failure
  cp "$build_dir/build-$variant/codex-qwen-tts" "$stage/$program"
done
cp native/qwen-tts/NOTICE.md native/qwen-tts/source.env "$stage/"
cp native/qwen-tts/licenses/* "$stage/licenses/"
find "$stage" -type d -exec chmod 755 {} +
find "$stage" -type f -exec chmod 644 {} +
chmod 755 "$stage/codex-qwen-tts" "$stage/lib/codex-qwen-tts-optimized"
[[ -z "$(find "$stage" -type l -print -quit)" ]] || { echo 'Qwen package contains symlinks' >&2; exit 1; }
"$stage/codex-qwen-tts" version
if [[ -n "${QWEN_TTS_MODEL_DIR:-}" ]]; then
  python3 native/qwen-tts/smoke.py --helper "$stage/codex-qwen-tts" --dir "$QWEN_TTS_MODEL_DIR" --language italian --threads 2
fi
(
  cd "$stage"
  while IFS= read -r file; do
    printf '%s  %s\n' "$(hash_file "$file")" "$file"
  done < <(find . -type f ! -name SHA256SUMS | sed 's|^./||' | LC_ALL=C sort) > SHA256SUMS
  chmod 644 SHA256SUMS
  find . -type f | sed 's|^./||' | LC_ALL=C sort > "$build_dir/archive-files"
  if [[ "$target_os" == darwin ]]; then
    COPYFILE_DISABLE=1 tar --format=ustar --uid 0 --gid 0 --uname root --gname root -czf "$output/codex-qwen-tts-$target_os-$target_arch.tar.gz" -T "$build_dir/archive-files"
  else
    tar --format=ustar --owner=0 --group=0 --numeric-owner --mtime='UTC 2026-01-01' -czf "$output/codex-qwen-tts-$target_os-$target_arch.tar.gz" -T "$build_dir/archive-files"
  fi
)
