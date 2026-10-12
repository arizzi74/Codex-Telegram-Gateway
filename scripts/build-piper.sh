#!/usr/bin/env bash
# Native host build using the exact official shared CPU runtime.
set -euo pipefail
cd "$(dirname "$0")/.."
source native/piper/source.env
target_os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in x86_64|amd64) target_arch=amd64 ;; aarch64|arm64) target_arch=arm64 ;; *) exit 1 ;; esac
case "$target_os/$target_arch" in linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) ;; *) exit 1 ;; esac
output="${PIPER_OUTPUT_DIR:-dist/piper}"
mkdir -p "$output"
output=$(cd "$output" && pwd)
build_dir=$(mktemp -d)
trap 'rm -rf -- "$build_dir"' EXIT
sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d ' ' -f 1
  else shasum -a 256 "$1" | cut -d ' ' -f 1; fi
}
fetch() {
  local url="$1" destination="$2" expected="$3" local_archive="$4"
  if [[ -n "$local_archive" ]]; then cp "$local_archive" "$destination"
  else
    curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 3 "$url" -o "$destination"
  fi
  [[ "$(sha256 "$destination")" == "$expected" ]] || { echo 'Piper source/runtime checksum mismatch' >&2; exit 1; }
}
case "$target_os/$target_arch" in
  linux/amd64) runtime="$PIPER_RUNTIME_LINUX_AMD64"; runtime_hash="$PIPER_RUNTIME_LINUX_AMD64_SHA256" ;;
  linux/arm64) runtime="$PIPER_RUNTIME_LINUX_ARM64"; runtime_hash="$PIPER_RUNTIME_LINUX_ARM64_SHA256" ;;
  darwin/amd64) runtime="$PIPER_RUNTIME_DARWIN_AMD64"; runtime_hash="$PIPER_RUNTIME_DARWIN_AMD64_SHA256" ;;
  darwin/arm64) runtime="$PIPER_RUNTIME_DARWIN_ARM64"; runtime_hash="$PIPER_RUNTIME_DARWIN_ARM64_SHA256" ;;
esac
fetch "$PIPER_SOURCE_URL" "$build_dir/source.tar.gz" "$PIPER_SOURCE_SHA256" "${PIPER_SOURCE_ARCHIVE:-}"
fetch "https://github.com/k2-fsa/sherpa-onnx/releases/download/v$PIPER_SHERPA_VERSION/$runtime.tar.bz2" "$build_dir/runtime.tar.bz2" "$runtime_hash" "${PIPER_RUNTIME_ARCHIVE:-}"
tar -xzf "$build_dir/source.tar.gz" -C "$build_dir"
tar -xjf "$build_dir/runtime.tar.bz2" -C "$build_dir"
cmake_args=(-DCMAKE_BUILD_TYPE=Release "-DCODEX_PIPER_VERSION=${VERSION:-dev}" "-DPIPER_SOURCE_DIR=$build_dir/sherpa-onnx-$PIPER_SHERPA_VERSION" "-DPIPER_RUNTIME_DIR=$build_dir/$runtime")
if [[ "$target_os" == darwin ]]; then cmake_args+=(-DCMAKE_OSX_DEPLOYMENT_TARGET=13.0); fi
cmake -S native/piper -B "$build_dir/build" "${cmake_args[@]}"
cmake --build "$build_dir/build" --parallel "${BUILD_JOBS:-2}"
ctest --test-dir "$build_dir/build" --output-on-failure

stage="$output/$target_os-$target_arch"
[[ ! -e "$stage" ]] || { echo 'Piper output stage already exists; choose a clean output directory' >&2; exit 1; }
mkdir -p "$stage/lib"
cp "$build_dir/build/codex-piper" "$stage/codex-piper"
cp native/piper/NOTICE.md native/piper/source.env "$stage/"
cp -R native/piper/licenses "$stage/licenses"
if [[ "$target_os" == linux ]]; then
  cp -L "$build_dir/$runtime/lib/libsherpa-onnx-c-api.so" "$build_dir/$runtime/lib/libonnxruntime.so" "$stage/lib/"
else
  cp -L "$build_dir/$runtime/lib/libsherpa-onnx-c-api.dylib" "$build_dir/$runtime/lib/libonnxruntime.dylib" "$stage/lib/"
  # Repair absolute/upstream build paths before signing the relocatable bundle.
  for library in "$stage/lib/"*.dylib; do
    install_name_tool -id "@rpath/$(basename "$library")" "$library"
  done
  for binary in "$stage/codex-piper" "$stage/lib/"*.dylib; do
    while IFS= read -r dependency; do
      name=$(basename "$dependency")
      case "$name" in
        libsherpa-onnx-c-api.dylib|libonnxruntime.dylib)
          if [[ "$binary" == "$stage/codex-piper" ]]; then replacement="@loader_path/lib/$name"
          else replacement="@loader_path/$name"; fi
          install_name_tool -change "$dependency" "$replacement" "$binary"
          ;;
      esac
    done < <(otool -L "$binary" | sed -n '2,$s/^[[:space:]]*\([^ ]*\).*/\1/p')
    codesign --force --sign - "$binary"
  done
fi
find "$stage" -type d -exec chmod 755 {} +
find "$stage" -type f -exec chmod 644 {} +
chmod 755 "$stage/codex-piper"
[[ -z "$(find "$stage" -type l -print -quit)" ]] || { echo 'Piper package contains symlinks' >&2; exit 1; }
"$stage/codex-piper" version
if [[ -n "${PIPER_MODEL_DIR:-}" ]]; then
  go run ./native/piper/smoke --helper "$stage/codex-piper" --dir "$PIPER_MODEL_DIR" --language italian --threads 2
  go run ./native/piper/smoke --helper "$stage/codex-piper" --dir "$PIPER_MODEL_DIR" --language english --threads 2
fi
(
  cd "$stage"
  while IFS= read -r file; do
    if command -v sha256sum >/dev/null; then sha256sum "$file"
    else shasum -a 256 "$file"; fi
  done < <(find codex-piper lib NOTICE.md licenses source.env -type f | LC_ALL=C sort) > SHA256SUMS
  chmod 644 SHA256SUMS
  if [[ "$target_os" == darwin ]]; then
    COPYFILE_DISABLE=1 tar --format=ustar --uid 0 --gid 0 --uname root --gname root -czf "$output/codex-piper-$target_os-$target_arch.tar.gz" codex-piper lib NOTICE.md licenses source.env SHA256SUMS
  else
    tar --format=ustar --owner=0 --group=0 --numeric-owner -czf "$output/codex-piper-$target_os-$target_arch.tar.gz" codex-piper lib NOTICE.md licenses source.env SHA256SUMS
  fi
)
