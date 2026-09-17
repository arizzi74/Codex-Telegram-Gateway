#!/bin/sh
# Download and execute the native, checksum-verified release manager.
set -eu

# The native manager selects gateway setup under sudo, worker setup otherwise.
# Explicit administration commands continue to be forwarded unchanged.
if [ "$#" -eq 0 ]; then
  set -- setup
fi

fail() { printf '%s\n' "Installation failed: $*" >&2; exit 1; }
stable_tag() {
  case "$1" in v*) tag_numbers=${1#v} ;; *) return 1 ;; esac
  tag_major=${tag_numbers%%.*}; tag_rest=${tag_numbers#*.}
  [ "$tag_rest" != "$tag_numbers" ] || return 1
  tag_minor=${tag_rest%%.*}; tag_patch=${tag_rest#*.}
  [ "$tag_patch" != "$tag_rest" ] || return 1
  for tag_part in "$tag_major" "$tag_minor" "$tag_patch"; do
    case "$tag_part" in ''|*[!0-9]*|0[0-9]*) return 1 ;; esac
  done
}
select_version() {
  stable_tag "$1" || fail '--version must select a stable vMAJOR.MINOR.PATCH release'
  [ -z "$release_tag" ] || [ "$release_tag" = "$1" ] || fail 'conflicting --version arguments'
  release_tag=$1
}
select_repo() {
  case "$1" in ''|*[!A-Za-z0-9_./-]*|/*|*/|*/*/*) fail '--repo must be OWNER/REPOSITORY' ;; esac
  case "$1" in */*) ;; *) fail '--repo must be OWNER/REPOSITORY' ;; esac
  for repo_part in "${1%%/*}" "${1#*/}"; do
    case "$repo_part" in .|..) fail '--repo must be OWNER/REPOSITORY' ;; esac
  done
  [ -z "$requested_repo" ] || [ "$requested_repo" = "$1" ] || fail 'conflicting --repo arguments'
  requested_repo=$1
  repository=$1
}

repository=arizzi74/Codex-Telegram-Gateway
requested_repo=
release_tag=
expect=
for argument do
  if [ -n "$expect" ]; then
    case "$expect" in version) select_version "$argument" ;; repo) select_repo "$argument" ;; esac
    expect=
    continue
  fi
  case "$argument" in
    --version) expect=version ;;
    --version=*) select_version "${argument#--version=}" ;;
    --repo) expect=repo ;;
    --repo=*) select_repo "${argument#--repo=}" ;;
  esac
done
[ -z "$expect" ] || fail "--$expect requires a value"
for utility in curl uname mktemp chmod rm; do
  command -v "$utility" >/dev/null 2>&1 || fail "$utility is required"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  fail 'sha256sum or shasum is required'
fi
case "$(uname -s)" in Linux) platform=linux ;; Darwin) platform=darwin ;; *) fail 'unsupported operating system' ;; esac
case "$(uname -m)" in x86_64|amd64) architecture=amd64 ;; aarch64|arm64) architecture=arm64 ;; *) fail 'unsupported CPU architecture' ;; esac
asset="codex-telegramgw-$platform-$architecture"
download() {
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --tlsv1.2 --connect-timeout 15 --max-time 180 --retry 2 "$@"
}
if [ -z "$release_tag" ]; then
  release_url=$(download --max-filesize 1048576 --output /dev/null --write-out '%{url_effective}' \
    "https://github.com/$repository/releases/latest") || fail 'could not resolve the latest release'
  case "$release_url" in
    "https://github.com/$repository/releases/tag/"*) release_tag=${release_url##*/} ;;
    *) fail 'GitHub returned an unexpected release URL' ;;
  esac
  stable_tag "$release_tag" || fail 'GitHub did not return a stable release tag'
  [ "$release_url" = "https://github.com/$repository/releases/tag/$release_tag" ] || fail 'GitHub returned an unexpected release URL'
fi

temporary=$(mktemp -d "${TMPDIR:-/tmp}/codex-telegramgw-install.XXXXXXXX")
trap 'rm -rf -- "$temporary"' EXIT
trap 'exit 1' HUP INT TERM
base="https://github.com/$repository/releases/download/$release_tag"
download --max-filesize 1048576 --output "$temporary/SHA256SUMS" "$base/SHA256SUMS" || fail 'could not download release checksums'
expected=
seen=' '
while IFS= read -r line || [ -n "$line" ]; do
  hash=${line%% *}
  [ "${#hash}" -eq 64 ] || fail 'malformed release checksum manifest'
  case "$hash" in *[!0-9a-fA-F]*) fail 'malformed release checksum manifest' ;; esac
  entry=${line#"$hash"}
  case "$entry" in '  '*) entry=${entry#'  '} ;; ' *'*) entry=${entry#' *'} ;; *) fail 'malformed release checksum manifest' ;; esac
  entry=${entry#./}
  case "$entry" in ''|.|..|*[!A-Za-z0-9_.-]*) fail 'unsafe name in release checksum manifest' ;; esac
  case "$seen" in *" $entry "*) fail 'duplicate name in release checksum manifest' ;; esac
  seen="$seen$entry "
  if [ "$entry" = "$asset" ]; then expected=$hash; fi
done < "$temporary/SHA256SUMS"
[ -n "$expected" ] || fail "release does not contain $asset; select a release with the native Go installer"
download --max-filesize 67108864 --output "$temporary/$asset" "$base/$asset" || fail 'could not download the release manager'
# Let the platform checksum utility accept both uppercase and lowercase hashes.
printf '%s  %s\n' "$expected" "$asset" > "$temporary/manager.sha256"
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$temporary" && sha256sum -c manager.sha256) >/dev/null 2>&1 || fail 'installer checksum verification failed; no installer code was executed'
else
  (cd "$temporary" && shasum -a 256 -c manager.sha256) >/dev/null 2>&1 || fail 'installer checksum verification failed; no installer code was executed'
fi
chmod 0700 "$temporary/$asset"
CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE=$release_tag
export CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE
"$temporary/$asset" "$@"
