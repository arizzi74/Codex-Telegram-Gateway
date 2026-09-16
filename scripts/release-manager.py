#!/usr/bin/env python3
"""Install and update Codex Telegram Gateway from verified GitHub Releases.

Python 3.10+ and the operating system service manager are the only installer
requirements. Configuration, enrollment and Codex authentication stay local.
"""
import argparse
import contextlib
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import plistlib
import pwd
import re
import shutil
import sqlite3
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_REPO = 'arizzi74/Codex-Telegram-Gateway'
GATEWAY_DATA_ROOT = Path('/var/lib/codex-gateway')
VERSION_RE = re.compile(r'^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$')
REPO_RE = re.compile(r'^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$')
MAX_ARCHIVE = 128 * 1024 * 1024
MAX_UNPACKED = 512 * 1024 * 1024


class UpdateError(Exception):
    pass


class Busy(UpdateError):
    pass


def version_tuple(value):
    match = VERSION_RE.fullmatch(value.strip())
    if not match:
        raise UpdateError('Release version must be a stable vMAJOR.MINOR.PATCH tag.')
    return tuple(map(int, match.groups()))


def run(command, capture=False, check=True):
    result = subprocess.run([str(arg) for arg in command], stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, text=True, timeout=90)
    if check and result.returncode:
        # Commands/configuration may carry secrets. Never include their output.
        raise UpdateError('A required command failed: ' + Path(str(command[0])).name)
    return result.stdout if capture else result


def fsync_dir(path):
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def atomic_write(destination, data, mode=0o600, owner=None):
    destination = Path(destination)
    destination.parent.mkdir(parents=True, exist_ok=True)
    if destination.is_symlink():
        raise UpdateError('Refusing to replace a symbolic link: ' + str(destination))
    old = destination.stat() if destination.exists() else None
    descriptor, temporary = tempfile.mkstemp(prefix='.' + destination.name + '.', dir=destination.parent)
    try:
        with os.fdopen(descriptor, 'wb') as output:
            output.write(data)
            os.fchmod(output.fileno(), stat.S_IMODE(old.st_mode) if old else mode)
            wanted_owner = (old.st_uid, old.st_gid) if old else owner
            if wanted_owner and os.geteuid() == 0:
                os.fchown(output.fileno(), *wanted_owner)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, destination)
        fsync_dir(destination.parent)
    finally:
        Path(temporary).unlink(missing_ok=True)


def atomic_copy(source, destination, mode=0o755, owner=None):
    atomic_write(destination, Path(source).read_bytes(), mode, owner)


def private_json(path, value):
    atomic_write(path, (json.dumps(value, indent=2) + '\n').encode())


def read_json(path):
    with open(path, encoding='utf-8') as source:
        value = json.load(source)
    if not isinstance(value, dict):
        raise UpdateError('Expected a JSON configuration object.')
    return value


@contextlib.contextmanager
def locked(path):
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor = os.open(path, os.O_CREAT | os.O_RDWR | getattr(os, 'O_NOFOLLOW', 0), 0o600)
    try:
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise Busy('Another installation or update is already running.') from error
        yield
    finally:
        os.close(descriptor)


class HTTPSRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, new_url):
        target = urllib.parse.urlsplit(new_url)
        hosts = {'github.com', 'api.github.com', 'release-assets.githubusercontent.com',
                 'objects.githubusercontent.com', 'github-releases.githubusercontent.com'}
        if target.scheme != 'https' or target.hostname not in hosts or target.username or target.password:
            raise UpdateError('Rejected an unexpected release download redirect.')
        return super().redirect_request(request, fp, code, message, headers, new_url)


def download(url, destination=None, limit=4 * 1024 * 1024):
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != 'https' or parsed.hostname not in {'api.github.com', 'github.com'} or parsed.username or parsed.password:
        raise UpdateError('Release source must be HTTPS on GitHub.')
    request = urllib.request.Request(url, headers={'User-Agent': 'codex-telegramgw-release-manager',
                                                  'Accept': 'application/vnd.github+json' if parsed.hostname == 'api.github.com' else 'application/octet-stream'})
    opener = urllib.request.build_opener(HTTPSRedirects)
    chunks = []
    size = 0
    started = time.monotonic()
    with opener.open(request, timeout=30) as response:
        while True:
            chunk = response.read(1024 * 1024)
            if not chunk:
                break
            size += len(chunk)
            if size > limit or time.monotonic() - started > 300:
                raise UpdateError('Release download exceeded its size or time limit.')
            chunks.append(chunk)
    data = b''.join(chunks)
    if destination:
        Path(destination).write_bytes(data)
    return data


def manifest(data, nested=False):
    hashes = {}
    for line in data.decode('ascii').splitlines():
        match = re.fullmatch(r'([0-9a-fA-F]{64}) [ *](?:\./)?([^\s]+)', line)
        if not match or match[2] in hashes or '\\' in match[2]:
            raise UpdateError('Invalid release checksum manifest.')
        name = PurePosixPath(match[2])
        if name.is_absolute() or '..' in name.parts or str(name) != match[2] or (not nested and '/' in match[2]):
            raise UpdateError('Invalid release checksum manifest.')
        hashes[match[2]] = match[1].lower()
    if not hashes:
        raise UpdateError('Empty release checksum manifest.')
    return hashes


def verify_file(path, digest):
    if hashlib.sha256(Path(path).read_bytes()).hexdigest() != digest:
        raise UpdateError('Release checksum verification failed.')


def unpack_verified(archive, target, binary):
    """Never let tarfile write paths or links from an untrusted archive."""
    total = 0
    seen = set()
    with tarfile.open(archive, 'r:gz') as package:
        for entry in package:
            name = entry.name.removeprefix('./')
            if name in ('', '.') and entry.isdir():
                continue
            path = PurePosixPath(name)
            if (path.is_absolute() or '..' in path.parts or '\\' in name or
                    str(path) != name.rstrip('/') or name in seen or
                    not (entry.isfile() or entry.isdir())):
                raise UpdateError('Unsafe entry in release archive.')
            seen.add(name)
            if len(seen) > 10000:
                raise UpdateError('Release archive has too many files.')
            total += entry.size
            if entry.size < 0 or total > MAX_UNPACKED:
                raise UpdateError('Release archive exceeds unpacked size limit.')
            output = target.joinpath(*path.parts)
            if entry.isdir():
                output.mkdir(parents=True, exist_ok=True)
            else:
                output.parent.mkdir(parents=True, exist_ok=True)
                with package.extractfile(entry) as source, output.open('xb') as destination:
                    shutil.copyfileobj(source, destination)
                output.chmod(0o755 if name == binary else 0o600)
    hashes = manifest((target / 'SHA256SUMS').read_bytes(), nested=True)
    if binary not in hashes or not (target / binary).is_file():
        raise UpdateError('Release archive is missing its verified binary.')
    verify_file(target / binary, hashes[binary])
    manager = 'scripts/release-manager.py'
    if manager not in hashes:
        raise UpdateError('Release archive is missing its verified updater.')
    verify_file(target / manager, hashes[manager])


class Release:
    def __init__(self, repo, version=None):
        if not REPO_RE.fullmatch(repo) or any(part in {'.', '..'} for part in repo.split('/')):
            raise UpdateError('Repository must be OWNER/REPOSITORY.')
        self.repo = repo
        suffix = 'latest'
        if version:
            version_tuple(version)
            suffix = 'tags/v' + version.removeprefix('v')
        data = json.loads(download('https://api.github.com/repos/' + repo + '/releases/' + suffix))
        if data.get('draft') or data.get('prerelease'):
            raise UpdateError('Only published stable releases can be installed.')
        self.tag = data.get('tag_name', '')
        version_tuple(self.tag)
        if self.tag != 'v' + self.tag.removeprefix('v'):
            raise UpdateError('Release tag must start with v.')
        if version and version_tuple(self.tag) != version_tuple(version):
            raise UpdateError('GitHub returned the wrong release version.')
        self.assets = {}
        prefix = 'https://github.com/' + repo + '/releases/download/' + self.tag + '/'
        for asset in data.get('assets', []):
            name = asset.get('name', '')
            if '/' in name or '\\' in name or name in self.assets:
                raise UpdateError('Invalid or duplicate release asset.')
            url = prefix + urllib.parse.quote(name, safe='')
            if asset.get('browser_download_url') != url:
                raise UpdateError('Release asset URL does not match the selected repository and tag.')
            self.assets[name] = url
        if 'SHA256SUMS' not in self.assets:
            raise UpdateError('Release is missing SHA256SUMS.')
        self.hashes = manifest(download(self.assets['SHA256SUMS'], limit=1024 * 1024))

    def package(self, binary, system, architecture, stage):
        name = binary + '-' + system + '-' + architecture + '.tar.gz'
        if name not in self.assets or name not in self.hashes:
            raise UpdateError('Release does not contain this platform: ' + name)
        archive = stage / name
        download(self.assets[name], archive, MAX_ARCHIVE)
        verify_file(archive, self.hashes[name])
        destination = stage / binary
        destination.mkdir()
        unpack_verified(archive, destination, binary)
        if binary != 'codex-local':
            actual = run([destination / binary, 'version'], capture=True).strip()
            if version_tuple(actual) != version_tuple(self.tag):
                raise UpdateError('Binary version does not match its release tag.')
        return destination


class Layout:
    def __init__(self, component):
        self.component = component
        self.system = {'Linux': 'linux', 'Darwin': 'darwin'}.get(platform.system())
        self.architecture = {'x86_64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}.get(platform.machine())
        if not self.system or not self.architecture or (component == 'gateway' and self.system != 'linux'):
            raise UpdateError('Unsupported component or platform.')
        self.home = Path.home()
        if any(c.isspace() or c in '%\"\\' for c in str(self.home)):
            raise UpdateError('Service installation requires a home path without spaces, quotes, percent signs or backslashes.')
        if component == 'gateway':
            self.bin = Path('/usr/local/lib/codex-telegramgw')
            self.manager = self.bin / 'release-manager.py'
            self.command = Path('/usr/local/bin/codex-telegramgw')
            self.config = Path('/etc/codex-gateway/gateway.json')
            self.environment = Path('/etc/codex-gateway/secrets.env')
            self.state = Path('/etc/codex-gateway/update.json')
            self.lock = Path('/run/lock/codex-telegramgw-update.lock')
            self.backups = Path('/var/backups/codex-gateway/updates')
            self.unit_dir = Path('/etc/systemd/system')
        else:
            self.bin = self.home / '.local/bin'
            self.manager = self.home / '.local/lib/codex-telegramgw/release-manager.py'
            self.command = self.bin / 'codex-telegramgw'
            self.config = self.home / '.config/codex-worker/config.json'
            self.environment = None
            self.state = self.home / '.config/codex-worker/update.json'
            self.lock = self.home / '.local/state/codex-worker/update.lock'
            self.backups = self.home / '.local/state/codex-worker/updates'
            self.unit_dir = self.home / '.config/systemd/user'
        self.binary = self.bin / ('codex-' + component)
        self.unit = self.unit_dir / ('codex-' + component + '.service')

    def require_user(self):
        if self.component == 'gateway' and os.geteuid() != 0:
            raise UpdateError('Gateway installation and updates require sudo.')
        if self.component == 'worker' and os.geteuid() == 0:
            raise UpdateError('Run worker installation and updates as the worker account, without sudo.')
        # All root-trusted installer state remains outside service-writable data.
        if self.component == 'gateway':
            for path in (self.state, self.config, self.config.parent, self.manager.parent):
                if path.exists() and (path.is_symlink() or path.stat().st_uid != 0 or path.stat().st_mode & 0o022):
                    raise UpdateError('Gateway installer paths must be root-owned and not group/world writable.')

    def service(self, action, check=True):
        if self.system == 'linux':
            prefix = ['systemctl'] + (['--user'] if self.component == 'worker' else [])
            return run(prefix + [action, 'codex-' + self.component + '.service'], check=check)
        label = 'gui/' + str(os.getuid()) + '/com.iaia.codex-worker'
        plist = self.home / 'Library/LaunchAgents/com.iaia.codex-worker.plist'
        if action == 'stop':
            return run(['launchctl', 'bootout', label], check=check)
        if action in ('start', 'restart', 'enable'):
            if action == 'restart':
                run(['launchctl', 'bootout', label], check=False)
            return run(['launchctl', 'bootstrap', 'gui/' + str(os.getuid()), plist], check=check)
        raise UpdateError('Unsupported launchd operation.')

    def installed_version(self):
        return run([self.binary, 'version'], capture=True).strip()


def install_manager(layout, source):
    atomic_copy(source, layout.manager)
    layout.command.parent.mkdir(parents=True, exist_ok=True)
    if layout.command == layout.manager:
        return
    if layout.command.exists() or layout.command.is_symlink():
        if not layout.command.is_symlink() or layout.command.resolve() != layout.manager.resolve():
            raise UpdateError('Manager command path is already occupied.')
        return
    layout.command.symlink_to(layout.manager)
    fsync_dir(layout.command.parent)


def secure_gateway_adoption(layout):
    if os.geteuid() != 0:
        raise UpdateError('Gateway adoption requires sudo.')
    account = pwd.getpwnam('codexgateway')
    if (layout.config.parent.is_symlink() or layout.config.is_symlink() or
            not layout.config.is_file() or not layout.environment.is_file() or layout.environment.is_symlink()):
        raise UpdateError('Gateway adoption requires regular configuration files in the standard directory.')
    cfg = read_json(layout.config)
    database = Path(cfg.get('database_path', ''))
    if not database.is_absolute():
        database = layout.config.parent / database
    if ('database_url_env' in cfg or not cfg.get('database_path') or database.is_symlink() or
            not database.resolve().is_relative_to(GATEWAY_DATA_ROOT)):
        raise UpdateError('Gateway adoption requires a SQLite database inside /var/lib/codex-gateway.')
    os.chown(layout.config.parent, 0, account.pw_gid)
    os.chmod(layout.config.parent, 0o750)
    os.chown(layout.config, 0, account.pw_gid)
    os.chmod(layout.config, 0o640)
    os.chown(layout.environment, 0, 0)
    os.chmod(layout.environment, 0o600)


def saved_settings(layout):
    value = read_json(layout.state)
    if value.get('component') != layout.component:
        raise UpdateError('Installer state belongs to another component.')
    return value


def save_settings(layout, repo, version):
    private_json(layout.state, {'schema': 1, 'component': layout.component, 'repo': repo,
                               'version': version, 'updated_at': datetime.datetime.now(datetime.timezone.utc).isoformat()})


def normalize_gateway(layout, source, environment):
    cfg = read_json(source)
    if 'database_url_env' in cfg or not cfg.get('database_path'):
        raise UpdateError('Provide a SQLite gateway configuration; PostgreSQL migration is a separate operation.')
    database = Path(cfg['database_path'])
    if not database.is_absolute():
        database = source.parent / database
    database = database.resolve()
    if not database.is_relative_to(GATEWAY_DATA_ROOT):
        raise UpdateError('Gateway database_path must be inside /var/lib/codex-gateway for the hardened service.')
    bot = Path(cfg.get('bot_secrets_file', '.botsecrets'))
    if not bot.is_absolute():
        bot = source.parent / bot
    if bot.stat().st_mode & 0o077:
        raise UpdateError('Bot secrets file must have mode 0600.')
    if not environment or not environment.is_file():
        raise UpdateError('Gateway install requires --secrets-env PATH.')
    cfg['database_path'] = str(database)
    cfg['bot_secrets_file'] = '/etc/codex-gateway/.botsecrets'
    if not cfg.get('webhook_secret_env'):
        raise UpdateError('Gateway configuration must name webhook_secret_env.')
    account = pwd.getpwnam('codexgateway')
    layout.config.parent.mkdir(parents=True, exist_ok=True, mode=0o750)
    os.chown(layout.config.parent, 0, account.pw_gid)
    os.chmod(layout.config.parent, 0o750)
    for directory in [GATEWAY_DATA_ROOT, *reversed(database.parent.parents), database.parent]:
        if directory == GATEWAY_DATA_ROOT or directory.is_relative_to(GATEWAY_DATA_ROOT):
            directory.mkdir(exist_ok=True, mode=0o700)
            os.chown(directory, account.pw_uid, account.pw_gid)
            os.chmod(directory, 0o700)
    for destination, data in ((layout.config, (json.dumps(cfg, indent=2) + '\n').encode()),
                              (layout.config.parent / '.botsecrets', bot.read_bytes())):
        atomic_write(destination, data, 0o640, (0, account.pw_gid))
        os.chmod(destination, 0o640 if destination == layout.config else 0o600)
        if destination != layout.config:
            os.chown(destination, account.pw_uid, account.pw_gid)
    atomic_write(layout.environment, environment.read_bytes(), 0o600, (0, 0))


def worker_executable_paths(cfg):
    paths = []
    for profile in cfg.get('runtimes', []):
        executable = profile['codex_binary']
        if '/' not in executable:
            executable = shutil.which(executable)
            if not executable:
                raise UpdateError('Codex executable is unavailable in the installing account PATH.')
            profile['codex_binary'] = executable
        if not os.access(executable, os.X_OK):
            raise UpdateError('Configured Codex executable is not executable.')
        paths.append(str(Path(executable).parent))
    node = shutil.which('node')
    if node:
        paths.append(str(Path(node).parent))
    paths.extend([str(Path.home() / '.local/bin'), '/opt/homebrew/bin', '/usr/local/bin', '/usr/bin', '/bin'])
    paths = list(dict.fromkeys(paths))
    if any(any(c in path for c in '\n\r%\"\\:') for path in paths):
        raise UpdateError('Executable directories contain unsupported service PATH characters.')
    return ':'.join(paths)


def install_configuration(layout, source, environment, package):
    if layout.config.exists():
        raise UpdateError('Existing configuration found; use adopt to manage this installation without replacing configuration.')
    if layout.component == 'gateway':
        try:
            pwd.getpwnam('codexgateway')
        except KeyError:
            run(['useradd', '--system', '--user-group', '--home-dir', '/var/lib/codex-gateway', '--shell', '/usr/sbin/nologin', 'codexgateway'])
        normalize_gateway(layout, source, environment)
    else:
        cfg = json.loads(run([package / 'codex-worker', '--config', source, 'config', 'export'], capture=True))
        worker_executable_paths(cfg)
        token = Path(cfg['token_file'])
        private_token = layout.config.parent / 'worker.token'
        atomic_write(private_token, token.read_bytes(), 0o600)
        cfg['token_file'] = str(private_token)
        private_json(layout.config, cfg)
        Path(cfg['state_file']).parent.mkdir(parents=True, exist_ok=True, mode=0o700)


def install_service(layout, package):
    if layout.system == 'linux':
        template = package / 'deploy/systemd' / layout.unit.name
        data = template.read_text()
        if layout.component == 'worker':
            service_path = worker_executable_paths(read_json(layout.config))
            data = re.sub(r'^Environment=PATH=.*$', 'Environment="PATH=' + service_path + '"', data, flags=re.MULTILINE)
        atomic_write(layout.unit, data.encode(), 0o644)
        run(['systemctl'] + (['--user'] if layout.component == 'worker' else []) + ['daemon-reload'])
    else:
        command = [str(layout.binary), '--config', str(layout.config), 'run']
        value = {'Label': 'com.iaia.codex-worker', 'ProgramArguments': command,
                 'EnvironmentVariables': {'HOME': str(layout.home), 'PATH': worker_executable_paths(read_json(layout.config))},
                 'WorkingDirectory': str(layout.home), 'RunAtLoad': True, 'KeepAlive': True,
                 'StandardOutPath': str(layout.home / 'Library/Logs/codex-worker.log'),
                 'StandardErrorPath': str(layout.home / 'Library/Logs/codex-worker.log')}
        (layout.home / 'Library/Logs').mkdir(parents=True, exist_ok=True)
        plist = layout.home / 'Library/LaunchAgents/com.iaia.codex-worker.plist'
        atomic_write(plist, plistlib.dumps(value), 0o644)


def sqlite_backup(database, backup):
    with sqlite3.connect(database.as_uri() + '?mode=ro', uri=True) as source, sqlite3.connect(backup) as target:
        source.backup(target)
        if target.execute('PRAGMA integrity_check').fetchall() != [('ok',)]:
            raise UpdateError('SQLite backup integrity check failed.')
    with open(backup, 'rb') as file:
        os.fsync(file.fileno())


def restore_sqlite(database, backup, existed):
    # The service owns its database directory. Perform rollback writes with that
    # identity, so even a replaced directory/link cannot turn them into root writes.
    data = backup.read_bytes() if backup.exists() else None
    def restore():
        for suffix in ('-wal', '-shm'):
            Path(str(database) + suffix).unlink(missing_ok=True)
        if data is not None:
            atomic_write(database, data, 0o600)
        elif not existed:
            database.unlink(missing_ok=True)
    if os.geteuid() != 0:
        restore()
        return
    account = pwd.getpwnam('codexgateway')
    pid = os.fork()
    if pid == 0:
        try:
            os.setgroups([])
            os.setgid(account.pw_gid)
            os.setuid(account.pw_uid)
            restore()
        except Exception:
            os._exit(1)
        os._exit(0)
    _, status = os.waitpid(pid, 0)
    if not os.WIFEXITED(status) or os.WEXITSTATUS(status):
        raise UpdateError('SQLite rollback could not complete; inspect the private backup before restarting.')


def gateway_service_active(layout):
    details = run(['systemctl', 'show', 'codex-gateway.service', '-p', 'MainPID', '-p', 'ActiveState'], capture=True)
    values = dict(line.split('=', 1) for line in details.splitlines() if '=' in line)
    pid = int(values.get('MainPID', 0))
    if values.get('ActiveState') != 'active' or pid <= 0:
        return False
    try:
        return os.path.samefile('/proc/' + str(pid) + '/exe', layout.binary)
    except OSError:
        return False


def gateway_ready(layout, timeout=45):
    cfg = read_json(layout.config)
    listen = cfg.get('listen', '127.0.0.1:8080')
    host, port = listen.rsplit(':', 1)
    if host in ('', '0.0.0.0', '[::]'):
        host = '127.0.0.1'
    url = 'http://' + host + ':' + port + '/readyz'
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                if response.status == 200 and gateway_service_active(layout):
                    return
        except (urllib.error.URLError, TimeoutError, OSError, UpdateError, ValueError):
            pass
        time.sleep(1)
    raise UpdateError('Gateway did not become ready; new database and binaries were retained. Inspect service logs.')


def timestamp(value):
    value = value.replace('Z', '+00:00')
    value = re.sub(r'\.(\d+)', lambda match: '.' + match[1][:6].ljust(6, '0'), value)
    return datetime.datetime.fromisoformat(value).timestamp()


def worker_running(layout):
    if layout.system == 'linux':
        details = run(['systemctl', '--user', 'show', 'codex-worker.service', '-p', 'MainPID', '-p', 'ActiveState'], capture=True)
        values = dict(line.split('=', 1) for line in details.splitlines() if '=' in line)
        if values.get('ActiveState') == 'active' and int(values.get('MainPID', 0)) > 0:
            return True
        if values.get('ActiveState') not in ('inactive', 'failed') or values.get('MainPID') != '0':
            raise Busy('Worker service is changing state; retry the update later.')
    else:
        service = run(['launchctl', 'print', 'gui/' + str(os.getuid()) + '/com.iaia.codex-worker'], check=False)
        if service.returncode == 0:
            return True
    result = run([layout.binary, '--config', layout.config, 'status'], check=False)
    if result.returncode == 0:
        try:
            pid = int(json.loads(result.stdout)['pid'])
            if pid <= 0:
                raise ValueError()
            os.kill(pid, 0)
        except ProcessLookupError:
            return False
        except (ValueError, KeyError, PermissionError):
            raise Busy('Cannot confirm that the old worker process has stopped.')
        raise Busy('A worker process is still running outside the managed service.')
    # A never-started installation legitimately has no status file.
    cfg = read_json(layout.config)
    status = Path(cfg['state_file'] + '.status.json')
    if not status.is_absolute():
        status = layout.config.parent / status
    if status.exists():
        raise Busy('Cannot confirm the stopped worker status.')
    return False


def worker_prepare(layout):
    result = run([layout.binary, '--config', layout.config, 'update', 'prepare'], check=False)
    if result.returncode:
        raise Busy('Worker update deferred: running/waiting work or no safe update support. See installation documentation.')
    token = None
    try:
        value = json.loads(result.stdout)
        token = value.get('token')
        expiry = timestamp(value['expires_at'])
        cfg = read_json(layout.config)
        if (not isinstance(token, str) or not token or int(value['pid']) < 1 or
                value['worker_id'] != cfg['worker_id'] or expiry - time.time() < 45):
            raise ValueError()
        if layout.system == 'linux':
            pid = run(['systemctl', '--user', 'show', 'codex-worker.service', '-p', 'MainPID', '--value'], capture=True).strip()
            if int(pid) != int(value['pid']):
                raise ValueError()
        if expiry - time.time() < 45:
            raise ValueError()
        return value
    except (ValueError, TypeError, KeyError, UpdateError) as error:
        if token:
            worker_abort(layout, token)
        raise UpdateError('Worker did not acknowledge preparation for the managed service process.') from error


def worker_abort(layout, token):
    run([layout.binary, '--config', layout.config, 'update', 'abort', '--token', token], check=False)


def worker_ready(layout, after, timeout=45, expected_version=None):
    expected_count = sum(1 for runtime in read_json(layout.config).get('runtimes', []) if runtime.get('autostart', False))
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            value = json.loads(run([layout.binary, '--config', layout.config, 'status'], capture=True))
            when = timestamp(value['updated_at'])
            runtimes = value.get('runtimes', [])
            if (when >= after and value.get('gateway_connected') and
                    (expected_version is None or version_tuple(value.get('version', '')) == version_tuple(expected_version)) and
                    len(runtimes) == expected_count and
                    all(r.get('state') == 'running' for r in runtimes)):
                return
        except (UpdateError, KeyError, ValueError):
            pass
        time.sleep(1)
    raise UpdateError('Worker did not become ready; inspect its service logs.')


def apply_update(layout, packages, release, fresh=False):
    """Restore only before starting any new service; later writes must survive."""
    layout.bin.mkdir(parents=True, exist_ok=True, mode=0o755)
    if layout.component == 'gateway':
        os.chmod(layout.bin, 0o755)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    backup = layout.backups / (stamp + '-' + str(os.getpid()))
    backup.mkdir(parents=True, mode=0o700)
    os.chmod(layout.backups, 0o700)
    sources = {layout.binary: packages[layout.component] / layout.binary.name,
               layout.manager: packages[layout.component] / 'scripts/release-manager.py'}
    if layout.component == 'worker':
        sources[layout.bin / 'codex-local'] = packages['local'] / 'codex-local'
    original = {}
    original_settings = layout.state.read_bytes() if layout.state.exists() else None
    for destination in sources:
        if destination.exists():
            copy = backup / destination.name
            shutil.copy2(destination, copy)
            original[destination] = copy
    database = None
    database_existed = False
    database_mutated = False
    replaced = set()
    settings_changed = False
    if layout.component == 'gateway':
        cfg = read_json(layout.config)
        if 'database_url_env' in cfg or not cfg.get('database_path'):
            raise UpdateError('This updater only supports an existing SQLite gateway.')
        database = Path(cfg['database_path'])
        if not database.is_absolute():
            database = layout.config.parent / database
        if database.is_symlink() or not database.resolve().is_relative_to(GATEWAY_DATA_ROOT):
            raise UpdateError('Gateway database must be a regular SQLite file inside /var/lib/codex-gateway.')
        database = database.resolve()
        database_existed = database.exists()
    prepared = False
    stopped = False
    started = False
    try:
        running = not fresh
        if not fresh and layout.component == 'worker':
            running = worker_running(layout)
            if running:
                prepared = worker_prepare(layout)
        if running and prepared:
            if timestamp(prepared['expires_at']) - time.time() < 45:
                raise Busy('Worker update reservation expired; retry later.')
        if running:
            stopped = True
            layout.service('stop')
        if database_existed:
            sqlite_backup(database, backup / 'gateway.db')
        for destination, source in sources.items():
            replaced.add(destination)
            atomic_copy(source, destination)
        install_manager(layout, sources[layout.manager])
        if layout.component == 'gateway':
            database_mutated = True
            run(['runuser', '-u', 'codexgateway', '--', layout.binary, '--config', layout.config, 'migrate'])
        # Crossing this boundary forbids restoring an older database or binary.
        settings_changed = True
        private_json(layout.state, {'schema': 1, 'component': layout.component, 'repo': release.repo,
                                    'version': release.tag, 'pending': True})
        started = True
        after = time.time()
        layout.service('start')
        if layout.component == 'gateway':
            gateway_ready(layout)
        else:
            worker_ready(layout, after, expected_version=release.tag)
        save_settings(layout, release.repo, release.tag)
    except Exception as error:
        error.service_started = started
        if not started:
            if settings_changed:
                if original_settings is not None:
                    atomic_write(layout.state, original_settings)
                else:
                    layout.state.unlink(missing_ok=True)
            for destination in replaced:
                if destination in original:
                    atomic_copy(original[destination], destination)
                else:
                    destination.unlink(missing_ok=True)
            if database_mutated:
                restore_sqlite(database, backup / 'gateway.db', database_existed)
            if stopped:
                layout.service('start')
            elif prepared:
                worker_abort(layout, prepared['token'])
        else:
            print('New service startup was attempted. No automatic rollback was performed; backup: ' + str(backup), file=sys.stderr)
        raise
    print('Installed ' + layout.component + ' ' + release.tag + '. Backup: ' + str(backup))


def fresh_install(layout, source, environment, packages, release):
    service_path = layout.unit if layout.system == 'linux' else layout.home / 'Library/LaunchAgents/com.iaia.codex-worker.plist'
    if layout.config.exists() or service_path.exists():
        raise UpdateError('Existing configuration/service found; use adopt to manage this installation.')
    paths = [layout.config, service_path, layout.state, layout.command, layout.manager,
             layout.config.parent / ('worker.token' if layout.component == 'worker' else '.botsecrets')]
    if layout.environment:
        paths.append(layout.environment)
    existed = {path: path.exists() or path.is_symlink() for path in paths}
    try:
        install_configuration(layout, source, environment, packages[layout.component])
        install_service(layout, packages[layout.component])
        if layout.system == 'linux':
            layout.service('enable')
        apply_update(layout, packages, release, True)
    except Exception as error:
        if not getattr(error, 'service_started', False):
            if service_path.exists() and not existed[service_path]:
                if layout.system == 'linux':
                    prefix = ['systemctl'] + (['--user'] if layout.component == 'worker' else [])
                    run(prefix + ['disable', '--now', service_path.name], check=False)
                else:
                    layout.service('stop', check=False)
            for path, present in existed.items():
                if not present:
                    path.unlink(missing_ok=True)
            if layout.system == 'linux':
                run(['systemctl'] + (['--user'] if layout.component == 'worker' else []) + ['daemon-reload'], check=False)
        raise


def auto_update(layout, enabled):
    label = 'codex-' + layout.component + '-update'
    if layout.system == 'linux':
        service = ('[Unit]\nDescription=Update Codex Telegram ' + layout.component + ' from GitHub Releases\n'
                   'Wants=network-online.target\nAfter=network-online.target\n\n[Service]\nType=oneshot\n'
                   'ExecStart=' + str(layout.command) + ' update ' + layout.component + '\n'
                   'SuccessExitStatus=75\nUMask=0077\nTimeoutStartSec=15min\n')
        timer = ('[Unit]\nDescription=Check daily for Codex Telegram ' + layout.component + ' releases\n\n'
                 '[Timer]\nOnCalendar=daily\nRandomizedDelaySec=30min\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n')
        prefix = ['systemctl'] + (['--user'] if layout.component == 'worker' else [])
        if enabled:
            atomic_write(layout.unit_dir / (label + '.service'), service.encode(), 0o644)
            atomic_write(layout.unit_dir / (label + '.timer'), timer.encode(), 0o644)
            run(prefix + ['daemon-reload'])
            run(prefix + ['enable', '--now', label + '.timer'])
        else:
            run(prefix + ['disable', '--now', label + '.timer'])
    else:
        label = 'com.iaia.codex-worker-update'
        plist = layout.home / 'Library/LaunchAgents' / (label + '.plist')
        domain = 'gui/' + str(os.getuid())
        run(['launchctl', 'bootout', domain + '/' + label], check=False)
        if enabled:
            value = {'Label': label, 'ProgramArguments': [str(layout.command), 'update', 'worker'],
                     'StartCalendarInterval': {'Hour': 4, 'Minute': 15},
                     'EnvironmentVariables': {'HOME': str(layout.home), 'PATH': str(layout.bin) + ':/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin'}}
            atomic_write(plist, plistlib.dumps(value), 0o644)
            run(['launchctl', 'bootstrap', domain, plist])
        else:
            plist.unlink(missing_ok=True)
    print('Daily automatic updates ' + ('enabled' if enabled else 'disabled') + ' for ' + layout.component + '.')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    actions = parser.add_subparsers(dest='action', required=True)
    for name in ('install', 'adopt', 'update'):
        command = actions.add_parser(name)
        command.add_argument('component', choices=('gateway', 'worker'))
        command.add_argument('--repo', default=None)
        if name != 'adopt':
            command.add_argument('--version', default=os.environ.get('CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE') if name == 'install' else None)
        if name == 'install':
            command.add_argument('--config', type=Path, required=True)
            command.add_argument('--secrets-env', type=Path)
        if name in ('install', 'adopt'):
            command.add_argument('--auto-update', action='store_true')
        else:
            command.add_argument('--check', action='store_true')
    automatic = actions.add_parser('auto-update')
    automatic.add_argument('setting', choices=('enable', 'disable'))
    automatic.add_argument('component', choices=('gateway', 'worker'))
    args = parser.parse_args(argv)
    os.umask(0o077)
    layout = Layout(args.component)
    if args.action == 'adopt' and args.component == 'gateway':
        secure_gateway_adoption(layout)
    layout.require_user()
    with locked(layout.lock):
        if args.action == 'auto-update':
            saved_settings(layout)
            auto_update(layout, args.setting == 'enable')
            return
        if args.action == 'adopt':
            if not layout.config.is_file() or not layout.binary.is_file():
                raise UpdateError('No existing installation found in the standard paths.')
            installed = layout.installed_version()
            version_tuple(installed)
            if layout.component == 'gateway':
                cfg = read_json(layout.config)
                if not cfg.get('database_path') or 'database_url_env' in cfg:
                    raise UpdateError('Only SQLite gateway installations can be adopted.')
            install_manager(layout, Path(__file__).resolve())
            save_settings(layout, args.repo or DEFAULT_REPO, 'v' + installed.removeprefix('v'))
            if args.auto_update:
                auto_update(layout, True)
            print('Adopted existing ' + layout.component + ' ' + installed + '; service was not restarted.')
            return
        settings = saved_settings(layout) if args.action == 'update' else {}
        repo = args.repo or settings.get('repo') or DEFAULT_REPO
        release = Release(repo, args.version)
        if args.action == 'update':
            installed = layout.installed_version()
            if version_tuple(release.tag) < version_tuple(installed):
                raise UpdateError('Downgrades are not supported.')
            if version_tuple(release.tag) == version_tuple(installed):
                if settings.get('pending'):
                    if args.check:
                        raise UpdateError('The installed version is awaiting a successful readiness check; inspect service logs.')
                    if layout.component == 'gateway':
                        gateway_ready(layout)
                    else:
                        worker_ready(layout, time.time() - 15, expected_version=release.tag)
                    save_settings(layout, repo, release.tag)
                print(layout.component + ' is already at ' + release.tag + '.')
                return
            if args.check:
                print('Update available for ' + layout.component + ': ' + installed + ' -> ' + release.tag)
                return
        with tempfile.TemporaryDirectory(prefix='codex-telegramgw-release-') as temporary:
            stage = Path(temporary)
            packages = {args.component: release.package('codex-' + args.component, layout.system, layout.architecture, stage)}
            if args.component == 'worker':
                packages['local'] = release.package('codex-local', layout.system, layout.architecture, stage)
            fresh = args.action == 'install'
            if fresh:
                fresh_install(layout, args.config.resolve(), args.secrets_env.resolve() if args.secrets_env else None, packages, release)
            else:
                apply_update(layout, packages, release, False)
        if fresh and args.auto_update:
            auto_update(layout, True)


if __name__ == '__main__':
    try:
        main()
    except Busy as error:
        print(str(error), file=sys.stderr)
        sys.exit(75)
    except Exception as error:
        print(str(error) if isinstance(error, UpdateError) else 'Installation/update failed: ' + type(error).__name__, file=sys.stderr)
        sys.exit(1)
