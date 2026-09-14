#!/usr/bin/env bash
# Run as the worker's user from a normal terminal after the current turn ends.
set -euo pipefail
exec python3 - "$0" "$@" <<'PY'
import argparse, datetime, hashlib, json, os, pathlib, platform, re, subprocess, sys, tarfile, time

parser = argparse.ArgumentParser(prog=sys.argv[1], description="Install the verified 0.2.1 release on this Linux ARM64 host.")
parser.add_argument("--check", action="store_true", help="read-only release, service, and idle preflight")
parser.add_argument("--wait", action="store_true", help="wait up to 10 minutes for idle instead of refusing")
args = parser.parse_args(sys.argv[2:])
root, home = pathlib.Path(sys.argv[1]).resolve().parent.parent, pathlib.Path.home()
worker, gateway = home / ".local/bin/codex-worker", pathlib.Path("/usr/local/lib/codex-telegramgw/codex-gateway")
def fail(message):
    sys.exit(message)
def run(command, **kwargs):
    return subprocess.run(command, check=True, **kwargs)
def worker_status():
    status = json.loads(subprocess.check_output([str(worker), "status"], stderr=subprocess.DEVNULL))
    pid = int(subprocess.check_output(["systemctl", "--user", "show", "codex-worker.service", "-p", "MainPID", "--value"]))
    stamp = re.sub(r"(\.\d{6})\d+", r"\1", status["updated_at"]).replace("Z", "+00:00")
    age = time.time() - datetime.datetime.fromisoformat(stamp).timestamp()
    if not pid or status["pid"] != pid or not 0 <= age <= 15:
        raise ValueError("Worker status is stale or does not match the running service")
    return status
def idle():
    try:
        return not any(s.get("active_turn_id") or s["state"].lower() in ("running", "active") or s["state"].lower().startswith("waiting") for s in worker_status()["sessions"])
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError):
        fail("Cannot verify worker status; deployment refused.")
deadline = time.monotonic() + (600 if args.wait else 0)
def await_idle():
    announced = False
    while not idle():
        if time.monotonic() >= deadline:
            fail("Worker has an active or waiting turn; retry after it ends, or use --wait (10 minutes maximum).")
        if not announced:
            print("Waiting for all worker turns to finish...", flush=True)
            announced = True
        time.sleep(2)
if os.geteuid() == 0 or platform.system() != "Linux" or platform.machine() not in ("aarch64", "arm64"):
    fail("Run as the worker's user on the Linux ARM64 host.")
if not args.check and "NoNewPrivs:\t1" in pathlib.Path("/proc/self/status").read_text():
    fail("This process cannot use sudo. Run this script from a normal terminal after the Codex turn ends.")
run([str(root / "scripts/verify-release.sh")], cwd=root, stdout=subprocess.DEVNULL)
for name in ("codex-gateway", "codex-worker", "codex-local"):
    source = root / "dist" / (name + "-linux-arm64")
    with tarfile.open(str(source) + ".tar.gz") as archive:
        if hashlib.sha256(archive.extractfile("./" + name).read()).digest() != hashlib.sha256(source.read_bytes()).digest():
            fail("Unpackaged release binary does not match its verified archive: " + name)
    if name != "codex-local" and subprocess.check_output([str(source), "version"], text=True).strip() != "0.2.1":
        fail("Build the 0.2.1 release before deploying.")
run(["systemctl", "is-active", "--quiet", "codex-gateway.service"])
run(["systemctl", "--user", "is-active", "--quiet", "codex-worker.service"])
await_idle()
if args.check:
    print("Release checksums and version verified; services are active and the worker is idle. No changes made.")
    sys.exit(0)
install = '''import os, pathlib, shutil, stat, sys, tempfile
root, home = map(pathlib.Path, sys.argv[1:])
for name, target in [("codex-gateway", pathlib.Path("/usr/local/lib/codex-telegramgw/codex-gateway")), ("codex-worker", home / ".local/bin/codex-worker"), ("codex-local", home / ".local/bin/codex-local")]:
    previous = target.lstat()
    if not stat.S_ISREG(previous.st_mode): raise RuntimeError("Installed binary must be a regular file")
    fd, temporary = tempfile.mkstemp(prefix=target.name + ".", dir=target.parent)
    try:
        with os.fdopen(fd, "wb") as output, (root / "dist" / (name + "-linux-arm64")).open("rb") as source:
            shutil.copyfileobj(source, output)
            os.fchown(output.fileno(), previous.st_uid, previous.st_gid)
            os.fchmod(output.fileno(), stat.S_IMODE(previous.st_mode))
            output.flush(); os.fsync(output.fileno())
        os.replace(temporary, target)
    finally:
        if os.path.exists(temporary): os.unlink(temporary)
'''
run(["sudo", "python3", "-c", install, str(root), str(home)])
await_idle()
run(["sudo", "systemctl", "restart", "codex-gateway.service"])
run(["systemctl", "is-active", "--quiet", "codex-gateway.service"])
time.sleep(6)  # Require a refreshed status after the gateway reconnect.
await_idle()
run(["systemctl", "--user", "restart", "codex-worker.service"])
run(["systemctl", "--user", "is-active", "--quiet", "codex-worker.service"])
for attempt in range(15):
    try:
        status = worker_status()
        if status["gateway_connected"] and status["event_ack"] == status["event_high"] and status["runtimes"] and all(r["state"] == "running" for r in status["runtimes"]):
            break
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError):
        pass
    time.sleep(2)
else:
    fail("Services restarted, but the worker did not reconnect with healthy runtimes and acknowledged events within 30 seconds.")
for binary in (gateway, worker):
    if subprocess.check_output([str(binary), "version"], text=True).strip() != "0.2.1":
        fail("Installed binary version verification failed: " + binary.name)
for pid in [status["pid"]] + [r["pid"] for r in status["runtimes"]]:
    flags = dict(line.split(":", 1) for line in pathlib.Path(f"/proc/{pid}/status").read_text().splitlines() if ":" in line)
    if flags.get("NoNewPrivs", "").strip() != "0":
        fail("Worker or Codex runtime still blocks privilege elevation after restart.")
run(["sudo", "-n", "/usr/bin/true"])
print("Installed 0.2.1; services active, worker connected, runtimes running, events acknowledged, and privilege restriction removed.")
PY
