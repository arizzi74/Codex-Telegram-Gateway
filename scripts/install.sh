#!/bin/sh
# Download a checksum-verified installer from a stable GitHub release.
set -eu
if ! command -v python3 >/dev/null 2>&1; then
  echo 'Python 3 is required. Install it, then run this installer again.' >&2
  exit 69
fi
python3 - "$@" <<'PY'
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

REPOSITORY = "arizzi74/Codex-Telegram-Gateway"
TAG_PATTERN = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
MANAGER_ASSET = "codex-telegramgw-manager.py"


class HTTPSRedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if urllib.parse.urlsplit(newurl).scheme != "https":
            raise ValueError("GitHub download attempted a non-HTTPS redirect")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def fetch(url, limit):
    if urllib.parse.urlsplit(url).scheme != "https":
        raise ValueError("release downloads require HTTPS")
    request = urllib.request.Request(url, headers={
        "User-Agent": "codex-telegramgw-installer",
        "Accept": "application/vnd.github+json" if "api.github.com/" in url else "application/octet-stream",
    })
    with urllib.request.build_opener(HTTPSRedirectHandler()).open(request, timeout=60) as response:
        content = response.read(limit + 1)
    if len(content) > limit:
        raise ValueError("GitHub release response exceeded its size limit")
    return content


def selected_tag(arguments):
    requested = []
    for index, argument in enumerate(arguments):
        if argument == "--version":
            if index + 1 >= len(arguments):
                raise ValueError("--version requires a stable release tag, such as v0.4.0")
            requested.append(arguments[index + 1])
        elif argument.startswith("--version="):
            requested.append(argument.split("=", 1)[1])
    if requested:
        if len(set(requested)) != 1 or not TAG_PATTERN.fullmatch(requested[0]):
            raise ValueError("--version must select one stable vMAJOR.MINOR.PATCH release")
        suffix = "tags/" + requested[0]
    else:
        suffix = "latest"
    metadata = json.loads(fetch(f"https://api.github.com/repos/{REPOSITORY}/releases/{suffix}", 1024 * 1024))
    tag = metadata.get("tag_name", "")
    if not isinstance(tag, str) or not TAG_PATTERN.fullmatch(tag):
        raise ValueError("GitHub did not return a stable release tag")
    if metadata.get("draft", True) or metadata.get("prerelease", True):
        raise ValueError("draft and prerelease builds cannot be installed")
    if requested and tag != requested[0]:
        raise ValueError("GitHub returned a different release than requested")
    return tag


def manager_digest(manifest):
    entries = {}
    for line in manifest.decode("utf-8").splitlines():
        if not line.strip():
            continue
        match = re.fullmatch(r"([0-9a-fA-F]{64}) [ *](?:\./)?([A-Za-z0-9_.-]+)", line)
        if not match or match[2] in entries:
            raise ValueError("release checksum manifest is malformed or contains duplicate names")
        entries[match[2]] = match[1].lower()
    if MANAGER_ASSET not in entries:
        raise ValueError("release checksum manifest does not include the installer")
    return entries[MANAGER_ASSET]


def main(arguments):
    tag = selected_tag(arguments)
    base = f"https://github.com/{REPOSITORY}/releases/download/{tag}/"
    expected = manager_digest(fetch(base + "SHA256SUMS", 1024 * 1024))
    manager = fetch(base + MANAGER_ASSET, 2 * 1024 * 1024)
    if hashlib.sha256(manager).hexdigest() != expected:
        raise ValueError("installer checksum verification failed; no installer code was executed")
    with tempfile.TemporaryDirectory(prefix="codex-telegramgw-install-") as temporary:
        path = Path(temporary) / MANAGER_ASSET
        path.write_bytes(manager)
        environment = os.environ.copy()
        environment["CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE"] = tag
        return subprocess.call([sys.executable, str(path), *arguments], env=environment)


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except (OSError, ValueError, urllib.error.URLError) as error:
        print(f"Installation failed: {error}", file=sys.stderr)
        sys.exit(1)
PY
