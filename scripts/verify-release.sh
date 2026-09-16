#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - <<'PY'
import hashlib
import re
import tarfile
from pathlib import Path

root = Path.cwd()
dist = root / "dist"
artifacts = {
    f"{binary}-{system}-{arch}.tar.gz": binary
    for system in ("linux", "darwin")
    for arch in ("amd64", "arm64")
    for binary in ("codex-gateway", "codex-worker", "codex-local")
    if binary != "codex-gateway" or system == "linux"
}
standalone = {
    "codex-telegramgw-manager.py": "scripts/release-manager.py",
    "install.sh": "scripts/install.sh",
}


def manifest(contents):
    entries = {}
    for line in contents.decode("utf-8").splitlines():
        match = re.fullmatch(r"([0-9a-fA-F]{64}) [ *](?:\./)?([A-Za-z0-9_./-]+)", line)
        if not match or match[2] in entries:
            raise SystemExit("invalid or duplicate SHA256SUMS entry")
        entries[match[2]] = match[1].lower()
    return entries


checksums = manifest((dist / "SHA256SUMS").read_bytes())
if set(checksums) != set(artifacts) | set(standalone):
    raise SystemExit("release manifest must contain exactly ten archives, the manager and installer")
if {path.name for path in dist.glob("*.tar.gz")} != set(artifacts):
    raise SystemExit("unexpected or missing platform archive")
for name, checksum in checksums.items():
    if hashlib.sha256((dist / name).read_bytes()).hexdigest() != checksum:
        raise SystemExit(f"release checksum mismatch: {name}")
for name, source in standalone.items():
    if (dist / name).read_bytes() != (root / source).read_bytes():
        raise SystemExit(f"release asset differs from source: {name}")
for name, binary in artifacts.items():
    with tarfile.open(dist / name, "r:gz") as archive:
        members = {}
        for member in archive.getmembers():
            normalized = member.name.removeprefix("./")
            if member.uid != 0 or member.gid != 0 or member.uname not in ("", "root") or member.gname not in ("", "root"):
                raise SystemExit(f"archive contains build-account ownership metadata: {name}: {normalized}")
            if normalized in members:
                raise SystemExit(f"duplicate archive member: {name}: {normalized}")
            if member.issym() or member.islnk() or member.name.startswith("/") or ".." in Path(normalized).parts:
                raise SystemExit(f"unsafe archive member: {name}: {normalized}")
            members[normalized] = member

        def contents(path):
            member = members.get(path)
            if member is None or not member.isfile():
                raise SystemExit(f"missing archive file: {name}: {path}")
            return archive.extractfile(member).read()

        inner = manifest(contents("SHA256SUMS"))
        if set(inner) != {binary, "scripts/release-manager.py", "scripts/install-worker.sh"}:
            raise SystemExit(f"unexpected inner checksum manifest: {name}")
        for path, checksum in inner.items():
            if hashlib.sha256(contents(path)).hexdigest() != checksum:
                raise SystemExit(f"archive checksum mismatch: {name}: {path}")
        if not members[binary].mode & 0o111:
            raise SystemExit(f"archive binary is not executable: {name}")
        for path in ("scripts/release-manager.py", "scripts/install-worker.sh", "scripts/migrate-postgres-to-sqlite.py",
                     "deploy/systemd/codex-worker.service", "deploy/systemd/codex-gateway.service"):
            if contents(path) != (root / path).read_bytes():
                raise SystemExit(f"archive file differs from source: {name}: {path}")
print("Verified ten platform archives and both installer assets.")
PY
