#!/bin/sh
# Preserve the historical installer URL through the canonical Twarm bootstrap.
set -eu
fail() { printf '%s\n' "Installation failed: $*" >&2; exit 1; }
for utility in curl mktemp rm; do
  command -v "$utility" >/dev/null 2>&1 || fail "$utility is required"
done
temporary=$(mktemp -d "${TMPDIR:-/tmp}/twarm-legacy-install.XXXXXXXX")
trap 'rm -rf -- "$temporary"' EXIT
trap 'exit 1' HUP INT TERM
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  --tlsv1.2 --connect-timeout 15 --max-time 180 --retry 2 --max-filesize 1048576 \
  --output "$temporary/install.sh" \
  https://raw.githubusercontent.com/arizzi74/Twarm/main/scripts/install.sh \
  || fail 'could not download the Twarm installer'
sh "$temporary/install.sh" "$@"
