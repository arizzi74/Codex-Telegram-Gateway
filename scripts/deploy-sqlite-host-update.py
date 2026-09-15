#!/usr/bin/env python3
"""Install a verified gateway release on the existing Linux systemd host.

Run with sudo from the repository. The worker and Codex runtime stay running.
Before first SQLite startup, failures restore the old gateway. After startup,
failures retain SQLite state and require inspection instead of a stale rollback.
"""
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import platform
import pwd
import shlex
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.request
import urllib.parse

ROOT = pathlib.Path(__file__).resolve().parent.parent
CONFIG = pathlib.Path('/etc/codex-gateway/gateway.json')
ENVIRONMENT = pathlib.Path('/etc/codex-gateway/secrets.env')
BINARY = pathlib.Path('/usr/local/lib/codex-telegramgw/codex-gateway')
UNIT = pathlib.Path('/etc/systemd/system/codex-gateway.service')


def run(command, **kwargs):
    return subprocess.run(command, check=True, **kwargs)


def output(command, **kwargs):
    return subprocess.check_output(command, text=True, stderr=subprocess.DEVNULL, **kwargs)


def atomic_copy(source, destination):
    previous = destination.stat()
    fd, temporary = tempfile.mkstemp(prefix='.' + destination.name + '.', dir=destination.parent)
    try:
        with os.fdopen(fd, 'wb') as target, open(source, 'rb') as origin:
            shutil.copyfileobj(origin, target)
            os.fchmod(target.fileno(), previous.st_mode & 0o777)
            os.fchown(target.fileno(), previous.st_uid, previous.st_gid)
            target.flush()
            os.fsync(target.fileno())
        os.replace(temporary, destination)
        directory = os.open(destination.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        pathlib.Path(temporary).unlink(missing_ok=True)


def postgres_environment(source):
    # libpq does not expand a connection URI placed directly in PGDATABASE.
    # Decode it into environment settings instead of exposing credentials in argv.
    uri = urllib.parse.urlsplit(source)
    if uri.scheme not in ('postgres', 'postgresql') or uri.fragment:
        raise ValueError('source database setting must be a PostgreSQL connection URI')
    environment = {'PATH': '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
                   'LANG': 'C.UTF-8', 'PGCONNECT_TIMEOUT': '10'}
    if uri.path and uri.path != '/':
        environment['PGDATABASE'] = urllib.parse.unquote(uri.path[1:])
    if uri.hostname:
        environment['PGHOST'] = uri.hostname
    if uri.port:
        environment['PGPORT'] = str(uri.port)
    if uri.username:
        environment['PGUSER'] = urllib.parse.unquote(uri.username)
    if uri.password is not None:
        environment['PGPASSWORD'] = urllib.parse.unquote(uri.password)
    names = {'host': 'PGHOST', 'hostaddr': 'PGHOSTADDR', 'port': 'PGPORT',
             'user': 'PGUSER', 'password': 'PGPASSWORD', 'dbname': 'PGDATABASE',
             'sslmode': 'PGSSLMODE', 'sslcert': 'PGSSLCERT', 'sslkey': 'PGSSLKEY',
             'sslrootcert': 'PGSSLROOTCERT', 'connect_timeout': 'PGCONNECT_TIMEOUT',
             'options': 'PGOPTIONS', 'service': 'PGSERVICE', 'passfile': 'PGPASSFILE'}
    for key, value in urllib.parse.parse_qsl(uri.query, keep_blank_values=True):
        if key not in names:
            raise ValueError('unsupported source connection option')
        environment[names[key]] = value
    return environment


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true', help='verify release and service preflight only')
    parser.add_argument('--worker-user', default=os.environ.get('SUDO_USER', ''), help='account running the worker')
    args = parser.parse_args()
    os.umask(0o077)
    if os.geteuid() != 0 or platform.system() != 'Linux' or not args.worker_user or args.worker_user == 'root':
        raise ValueError('run with sudo and specify the non-root --worker-user')
    arch = {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(platform.machine())
    if not arch:
        raise ValueError('unsupported host architecture')
    service = pwd.getpwnam('codexgateway')
    developer = pwd.getpwnam(args.worker_user)
    worker = pathlib.Path(developer.pw_dir) / '.local/bin/codex-worker'
    worker_command = ['runuser', '-u', developer.pw_name, '--', 'env', 'HOME=' + developer.pw_dir,
                      'XDG_RUNTIME_DIR=/run/user/' + str(developer.pw_uid), str(worker), 'status']

    def worker_status(after=0):
        status = json.loads(output(worker_command))
        # Never print session previews or other stored user content.
        stamp = status['updated_at'].replace('Z', '+00:00')
        import re
        stamp = re.sub(r'(\.\d{6})\d+', r'\1', stamp)
        age = time.time() - datetime.datetime.fromisoformat(stamp).timestamp()
        if not 0 <= age <= 20 or datetime.datetime.fromisoformat(stamp).timestamp() < after:
            raise ValueError('worker status is stale')
        return status

    run([str(ROOT / 'scripts/verify-release.sh')], cwd=ROOT, stdout=subprocess.DEVNULL)
    release = ROOT / 'dist' / ('codex-gateway-linux-' + arch)
    import tarfile
    with tarfile.open(str(release) + '.tar.gz') as archive:
        if hashlib.sha256(archive.extractfile('./codex-gateway').read()).digest() != hashlib.sha256(release.read_bytes()).digest():
            raise ValueError('gateway binary differs from verified archive')
    version = output([str(release), 'version']).strip()
    if version in ('', 'dev', '0.2.1', '0.2.0', '0.1.0'):
        raise ValueError('build a versioned SQLite release first')
    cfg = json.loads(CONFIG.read_text())
    run(['systemctl', 'is-active', '--quiet', 'codex-gateway.service'])
    before = worker_status()
    worker_pid = int(output(worker_command[:-2] + ['systemctl', '--user', 'show', 'codex-worker.service', '-p', 'MainPID', '--value']))
    if not worker_pid or before['pid'] != worker_pid:
        raise ValueError('worker status does not match the running service')
    if not before['gateway_connected']:
        raise ValueError('worker must be connected before deployment')
    migrating = 'database_url_env' in cfg
    database = pathlib.Path(cfg.get('database_path', '/var/lib/codex-gateway/gateway.db'))
    if not database.is_absolute():
        database = CONFIG.parent / database
    if migrating and (database.exists() or database.is_symlink()):
        raise ValueError('SQLite migration destination already exists')
    if args.check:
        print('Verified release ' + version + '; gateway active, worker connected; ' +
              ('PostgreSQL migration required.' if migrating else 'SQLite database already configured.'))
        return

    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    backup = pathlib.Path('/var/backups/codex-gateway') / stamp
    backup.mkdir(parents=True, mode=0o700)
    os.chmod(backup.parent, 0o700)
    originals = [(CONFIG, 'gateway.json'), (ENVIRONMENT, 'secrets.env'), (BINARY, 'codex-gateway'), (UNIT, 'gateway.service')]
    for original, name in originals:
        shutil.copy2(original, backup / name)
    env_lines = ENVIRONMENT.read_text().splitlines(keepends=True)
    if migrating:
        variable = cfg['database_url_env']
        source_url = None
        for line in env_lines:
            stripped = line.strip()
            if stripped.startswith(variable + '='):
                values = shlex.split(stripped.split('=', 1)[1])
                if len(values) != 1:
                    raise ValueError('invalid database environment value')
                source_url = values[0]
        if not source_url:
            raise ValueError('source database configuration is unavailable')
        pg_environment = postgres_environment(source_url)
        # Exercise the exact backup connection before stopping the live gateway.
        output(['runuser', '-u', service.pw_name, '--', 'psql', '-X', '-q', '-A', '-t', '-w', '-c', 'SELECT 1'],
               env=pg_environment)
        tools = database.parent / ('migration-tools-' + stamp)
        (tools / 'scripts').mkdir(parents=True, mode=0o700)
        shutil.copy2(ROOT / 'scripts/migrate-postgres-to-sqlite.py', tools / 'scripts/migrate-postgres-to-sqlite.py')
        shutil.copytree(ROOT / 'migrations', tools / 'migrations')
        for path in [tools, *tools.rglob('*')]:
            os.chown(path, service.pw_uid, service.pw_gid)
            os.chmod(path, 0o700 if path.is_dir() else 0o600)
    print('Stopping gateway; worker and Codex runtimes remain running.', flush=True)
    started = False
    try:
        run(['systemctl', 'stop', 'codex-gateway.service'])
        if migrating:
            with (backup / 'postgres.dump').open('xb') as dump:
                run(['runuser', '-u', service.pw_name, '--', 'pg_dump', '--no-password', '--format=custom'],
                    env=pg_environment, stdout=dump, stderr=subprocess.DEVNULL)
                dump.flush()
                os.fsync(dump.fileno())
            result = output(['runuser', '-u', service.pw_name, '--', 'python3',
                             str(tools / 'scripts/migrate-postgres-to-sqlite.py'), '--sqlite', str(database)], env=pg_environment)
            print(result.strip(), flush=True)
            cfg.pop('database_url_env')
            cfg['database_path'] = str(database)
            changed_env = ''.join(line for line in env_lines if not line.strip().startswith(variable + '='))
            (backup / 'new-secrets.env').write_text(changed_env)
            atomic_copy(backup / 'new-secrets.env', ENVIRONMENT)
        else:
            if not database.is_file():
                raise ValueError('configured SQLite database is missing')
            with sqlite3.connect(database.as_uri() + '?mode=ro', uri=True) as source, sqlite3.connect(backup / 'gateway.db') as target:
                source.backup(target)
        (backup / 'new-gateway.json').write_text(json.dumps(cfg, indent=2) + '\n')
        atomic_copy(backup / 'new-gateway.json', CONFIG)
        atomic_copy(release, BINARY)
        atomic_copy(ROOT / 'deploy/systemd/codex-gateway.service', UNIT)
        run(['systemctl', 'daemon-reload'])
        output(['runuser', '-u', service.pw_name, '--', str(BINARY), '--config', str(CONFIG), 'migrate'])
        # Once serve may accept new work, PostgreSQL is stale. Never revert it.
        started = True
        start_time = time.time()
        run(['systemctl', 'start', 'codex-gateway.service'])
        for _ in range(30):
            try:
                with urllib.request.urlopen('http://' + cfg['listen'] + '/readyz', timeout=2) as response:
                    ready = response.status == 200
                status = worker_status(after=start_time)
                if ready and status['gateway_connected'] and status['event_ack'] == status['event_high']:
                    break
            except Exception:
                pass
            time.sleep(2)
        else:
            raise ValueError('post-start readiness/reconnection check failed; SQLite retained for inspection')
        if before['pid'] != status['pid'] or sorted((r['runtime_id'], r['pid'], r['generation']) for r in before['runtimes']) != sorted((r['runtime_id'], r['pid'], r['generation']) for r in status['runtimes']):
            raise ValueError('worker/runtime identity changed during deployment; inspect services')
        if hashlib.sha256(BINARY.read_bytes()).digest() != hashlib.sha256(release.read_bytes()).digest():
            raise ValueError('installed binary checksum differs from release')
        with sqlite3.connect(database.as_uri() + '?mode=ro', uri=True) as check:
            if check.execute('PRAGMA integrity_check').fetchall() != [('ok',)] or check.execute('PRAGMA foreign_key_check').fetchone():
                raise ValueError('post-start database verification failed')
        print('Installed gateway ' + version + ' on SQLite; readiness healthy, worker reconnected, events acknowledged, runtime PIDs unchanged.')
        print('Private rollback backup: ' + str(backup))
    except Exception:
        if not started:
            for original, name in originals:
                atomic_copy(backup / name, original)
            run(['systemctl', 'daemon-reload'])
            run(['systemctl', 'start', 'codex-gateway.service'])
            print('Cutover failed before new startup; old gateway restored. Source database retained.', file=sys.stderr)
        raise


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # Subprocess arguments and database exceptions may contain private data.
        print('Deployment failed: ' + (str(error) if isinstance(error, ValueError) else type(error).__name__), file=sys.stderr)
        sys.exit(1)
