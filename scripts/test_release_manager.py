#!/usr/bin/env python3
"""Offline installer tests: no privileged services, GitHub or user data needed."""
import datetime
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tarfile
import tempfile
import types
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('release_manager', Path(__file__).with_name('release-manager.py'))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


def checksum(data):
    return hashlib.sha256(data).hexdigest()


class FakeLayout:
    def __init__(self, root, component='gateway'):
        self.component = component
        self.system = 'linux'
        self.home = root
        self.bin = root / 'bin'
        self.bin.mkdir()
        self.binary = self.bin / ('codex-' + component)
        self.binary.write_bytes(b'old binary')
        self.manager = root / 'lib/release-manager.py'
        self.manager.parent.mkdir()
        self.manager.write_bytes(b'old manager')
        self.command = self.bin / 'codex-telegramgw'
        self.config = root / 'etc/config.json'
        self.config.parent.mkdir()
        self.state = root / 'etc/update.json'
        self.state.write_text(json.dumps({'component': component, 'version': 'v1.0.0', 'repo': m.DEFAULT_REPO}))
        self.backups = root / 'backups'
        self.database = root / 'data/gateway.db'
        self.database.parent.mkdir()
        self.config.write_text(json.dumps({'database_path': str(self.database), 'worker_id': 'worker-id', 'state_file': str(root / 'worker.db')}))
        if component == 'gateway':
            with sqlite3.connect(self.database) as db:
                db.execute('CREATE TABLE data (value TEXT)')
                db.execute("INSERT INTO data VALUES ('original')")
        self.actions = []
        self.on_start = None

    def service(self, action, check=True):
        self.actions.append(action)
        if action == 'start' and self.on_start:
            self.on_start()


class ReleaseManagerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)

    def tearDown(self):
        self.temporary.cleanup()

    def package(self, extra=None, corrupt=False):
        binary = b'executable'
        manager = b'updater'
        hashes = (checksum(binary) + '  codex-gateway\n' + checksum(manager) + '  scripts/release-manager.py\n').encode()
        path = self.root / 'release.tar.gz'
        with tarfile.open(path, 'w:gz') as archive:
            for name, data in [('codex-gateway', binary + (b'x' if corrupt else b'')), ('scripts/release-manager.py', manager), ('SHA256SUMS', hashes)]:
                entry = tarfile.TarInfo('./' + name)
                entry.size = len(data)
                archive.addfile(entry, io.BytesIO(data))
            if extra:
                entry = tarfile.TarInfo(extra[0])
                if extra[1] == 'link':
                    entry.type = tarfile.SYMTYPE
                    entry.linkname = '/tmp/elsewhere'
                archive.addfile(entry)
        return path

    def test_safe_package_and_inner_checksums(self):
        target = self.root / 'stage'
        target.mkdir()
        m.unpack_verified(self.package(), target, 'codex-gateway')
        self.assertEqual((target / 'codex-gateway').read_bytes(), b'executable')
        self.assertEqual((target / 'scripts/release-manager.py').read_bytes(), b'updater')

    def test_rejects_checksum_tampering(self):
        target = self.root / 'stage'
        target.mkdir()
        with self.assertRaises(m.UpdateError):
            m.unpack_verified(self.package(corrupt=True), target, 'codex-gateway')

    def test_rejects_traversal_absolute_and_symlinks(self):
        for index, extra in enumerate([('../outside', 'file'), ('/tmp/outside', 'file'), ('alias', 'link')]):
            with self.subTest(extra=extra):
                target = self.root / ('stage' + str(index))
                target.mkdir()
                with self.assertRaises(m.UpdateError):
                    m.unpack_verified(self.package(extra), target, 'codex-gateway')
        self.assertFalse((self.root.parent / 'outside').exists())

    def test_manifest_nested_paths_are_only_allowed_inside_archives(self):
        line = (checksum(b'') + '  scripts/release-manager.py\n').encode()
        self.assertIn('scripts/release-manager.py', m.manifest(line, nested=True))
        with self.assertRaises(m.UpdateError):
            m.manifest(line)
        for name in ('../evil', '/evil', 'scripts/../evil'):
            with self.assertRaises(m.UpdateError):
                m.manifest((checksum(b'') + '  ' + name + '\n').encode(), nested=True)

    def test_manifest_normalizes_dot_prefix_and_rejects_duplicates(self):
        line = (checksum(b'') + '  ./thing.tar.gz\n').encode()
        self.assertIn('thing.tar.gz', m.manifest(line))
        with self.assertRaises(m.UpdateError):
            m.manifest(line + line)

    def test_atomic_replacement_preserves_mode_and_old_open_inode(self):
        path = self.root / 'program'
        path.write_bytes(b'old')
        path.chmod(0o751)
        with path.open('rb') as old:
            m.atomic_write(path, b'new', mode=0o600)
            self.assertEqual(old.read(), b'old')
        self.assertEqual(path.read_bytes(), b'new')
        self.assertEqual(path.stat().st_mode & 0o777, 0o751)

    def test_atomic_replacement_refuses_symlink(self):
        target = self.root / 'target'
        target.write_bytes(b'old')
        link = self.root / 'link'
        link.symlink_to(target)
        with self.assertRaises(m.UpdateError):
            m.atomic_write(link, b'new')
        self.assertEqual(target.read_bytes(), b'old')

    def test_concurrent_update_lock_rejected(self):
        with m.locked(self.root / 'lock'):
            with self.assertRaises(m.Busy):
                with m.locked(self.root / 'lock'):
                    self.fail('second lock acquired')

    def release_metadata(self):
        tag = 'v1.2.3'
        prefix = 'https://github.com/' + m.DEFAULT_REPO + '/releases/download/' + tag + '/'
        return {'tag_name': tag, 'draft': False, 'prerelease': False,
                'assets': [{'name': 'SHA256SUMS', 'browser_download_url': prefix + 'SHA256SUMS'}]}

    def test_release_uses_latest_stable_and_same_repo_assets(self):
        metadata = self.release_metadata()
        calls = []
        def fetch(url, **kwargs):
            calls.append(url)
            return json.dumps(metadata).encode() if '/releases/latest' in url else (checksum(b'') + '  codex.tar.gz\n').encode()
        with patch.object(m, 'download', side_effect=fetch):
            release = m.Release(m.DEFAULT_REPO)
        self.assertEqual(release.tag, 'v1.2.3')
        self.assertTrue(calls[0].endswith('/releases/latest'))

    def test_release_rejects_prerelease_or_wrong_repository(self):
        for mutate in (lambda value: value.update(prerelease=True),
                       lambda value: value['assets'][0].update(browser_download_url='https://github.com/attacker/repo/SHA256SUMS')):
            metadata = self.release_metadata()
            mutate(metadata)
            with patch.object(m, 'download', return_value=json.dumps(metadata).encode()):
                with self.assertRaises(m.UpdateError):
                    m.Release(m.DEFAULT_REPO)

    def test_release_rejects_pin_mismatch(self):
        with patch.object(m, 'download', return_value=json.dumps(self.release_metadata()).encode()):
            with self.assertRaises(m.UpdateError):
                m.Release(m.DEFAULT_REPO, 'v2.0.0')

    def test_https_download_rejects_arbitrary_hosts_before_opening(self):
        for url in ('http://github.com/x', 'https://evil.example/x', 'https://token@github.com/x'):
            with self.assertRaises(m.UpdateError):
                m.download(url)

    def test_stable_semver_only(self):
        self.assertEqual(m.version_tuple('v1.2.3'), (1, 2, 3))
        for value in ('dev', 'v1.2.3-rc1', 'v01.2.3', '1.2', '../1.2.3'):
            with self.assertRaises(m.UpdateError):
                m.version_tuple(value)

    def update_fixtures(self, component='gateway'):
        layout = FakeLayout(self.root, component)
        package = self.root / 'package'
        (package / 'scripts').mkdir(parents=True)
        (package / ('codex-' + component)).write_bytes(b'new binary')
        (package / 'scripts/release-manager.py').write_bytes(b'new manager')
        packages = {component: package}
        if component == 'worker':
            local = self.root / 'local'
            local.mkdir()
            (local / 'codex-local').write_bytes(b'new local')
            packages['local'] = local
        release = types.SimpleNamespace(repo=m.DEFAULT_REPO, tag='v1.2.3')
        return layout, packages, release

    def test_partial_backup_failure_preserves_database_wal_and_restarts_old_service(self):
        layout, packages, release = self.update_fixtures()
        connection = sqlite3.connect(layout.database)
        self.addCleanup(connection.close)
        connection.execute('PRAGMA journal_mode=WAL')
        connection.execute("UPDATE data SET value='committed in original WAL'")
        connection.commit()
        wal = Path(str(layout.database) + '-wal')
        database_before = layout.database.read_bytes()
        wal_before = wal.read_bytes()
        def partial_backup(_database, target):
            target.write_bytes(b'partial invalid backup')
            raise OSError('simulated disk full')
        with patch.object(m, 'GATEWAY_DATA_ROOT', self.root / 'data'), patch.object(m, 'sqlite_backup', side_effect=partial_backup), patch.object(m, 'restore_sqlite') as restore, patch.object(m, 'atomic_copy') as copy:
            with self.assertRaises(OSError):
                m.apply_update(layout, packages, release)
        restore.assert_not_called()
        copy.assert_not_called()
        self.assertEqual(layout.actions, ['stop', 'start'])
        self.assertEqual(layout.database.read_bytes(), database_before)
        self.assertEqual(wal.read_bytes(), wal_before)
        self.assertEqual(connection.execute('SELECT value FROM data').fetchone()[0], 'committed in original WAL')
        self.assertEqual(layout.binary.read_bytes(), b'old binary')

    def test_worker_readiness_requires_all_autostart_runtimes(self):
        layout = FakeLayout(self.root, 'worker')
        cfg = json.loads(layout.config.read_text())
        cfg['runtimes'] = [{'id': 'expected', 'autostart': True}, {'id': 'disabled', 'autostart': False}]
        layout.config.write_text(json.dumps(cfg))
        status = {'gateway_connected': True, 'updated_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                  'version': '1.2.3', 'runtimes': []}
        with patch.object(m, 'run', return_value=json.dumps(status)), patch.object(m.time, 'sleep'):
            with self.assertRaises(m.UpdateError):
                m.worker_ready(layout, 0, timeout=0.01, expected_version='v1.2.3')
        status['runtimes'] = [{'state': 'running'}]
        with patch.object(m, 'run', return_value=json.dumps(status)):
            m.worker_ready(layout, 0, timeout=0.01, expected_version='v1.2.3')
        cfg['runtimes'] = [{'id': 'disabled', 'autostart': False}]
        layout.config.write_text(json.dumps(cfg))
        status['runtimes'] = []
        with patch.object(m, 'run', return_value=json.dumps(status)):
            m.worker_ready(layout, 0, timeout=0.01, expected_version='v1.2.3')

    def test_failed_migration_restores_database_and_binaries_before_start(self):
        layout, packages, release = self.update_fixtures()
        config_before = layout.config.read_bytes()
        def migrate(_command):
            with sqlite3.connect(layout.database) as db:
                db.execute("UPDATE data SET value='migrated'")
            raise m.UpdateError('migration failed')
        with patch.object(m, 'GATEWAY_DATA_ROOT', self.root / 'data'), patch.object(m, 'run', side_effect=migrate):
            with self.assertRaises(m.UpdateError):
                m.apply_update(layout, packages, release)
        self.assertEqual(layout.actions, ['stop', 'start'])
        self.assertEqual(layout.binary.read_bytes(), b'old binary')
        self.assertEqual(layout.manager.read_bytes(), b'old manager')
        self.assertEqual(layout.config.read_bytes(), config_before)
        with sqlite3.connect(layout.database) as db:
            self.assertEqual(db.execute('SELECT value FROM data').fetchone()[0], 'original')

    def test_failed_readiness_never_restores_database_after_accepted_writes(self):
        layout, packages, release = self.update_fixtures()
        def accepted_write():
            with sqlite3.connect(layout.database) as db:
                db.execute("UPDATE data SET value='new user data'")
        layout.on_start = accepted_write
        with patch.object(m, 'GATEWAY_DATA_ROOT', self.root / 'data'), patch.object(m, 'run'), patch.object(m, 'gateway_ready', side_effect=m.UpdateError('not ready')):
            with self.assertRaises(m.UpdateError):
                m.apply_update(layout, packages, release)
        self.assertEqual(layout.binary.read_bytes(), b'new binary')
        self.assertTrue(json.loads(layout.state.read_text())['pending'])
        with sqlite3.connect(layout.database) as db:
            self.assertEqual(db.execute('SELECT value FROM data').fetchone()[0], 'new user data')

    def test_successful_gateway_update_preserves_config_and_marks_complete(self):
        layout, packages, release = self.update_fixtures()
        before = layout.config.read_bytes()
        with patch.object(m, 'GATEWAY_DATA_ROOT', self.root / 'data'), patch.object(m, 'run'), patch.object(m, 'gateway_ready'):
            m.apply_update(layout, packages, release)
        self.assertEqual(layout.config.read_bytes(), before)
        settings = json.loads(layout.state.read_text())
        self.assertEqual(settings['version'], 'v1.2.3')
        self.assertNotIn('pending', settings)

    def test_worker_busy_never_stops_or_replaces_binaries(self):
        layout, packages, release = self.update_fixtures('worker')
        with patch.object(m, 'worker_running', return_value=True), patch.object(m, 'worker_prepare', side_effect=m.Busy('busy')):
            with self.assertRaises(m.Busy):
                m.apply_update(layout, packages, release)
        self.assertEqual(layout.actions, [])
        self.assertEqual(layout.binary.read_bytes(), b'old binary')

    def test_stopped_legacy_worker_can_upgrade_without_preparation(self):
        layout, packages, release = self.update_fixtures('worker')
        with patch.object(m, 'worker_running', return_value=False), patch.object(m, 'worker_prepare') as prepare, patch.object(m, 'worker_ready'):
            m.apply_update(layout, packages, release)
        prepare.assert_not_called()
        self.assertEqual(layout.actions, ['start'])
        self.assertEqual((layout.bin / 'codex-local').read_bytes(), b'new local')

    def test_worker_preparation_checks_identity_and_lease(self):
        layout = FakeLayout(self.root, 'worker')
        lease = {'worker_id': 'worker-id', 'pid': 123, 'token': 'lease-token',
                 'expires_at': (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=120)).isoformat()}
        with patch.object(m, 'run', side_effect=[subprocess.CompletedProcess([], 0, json.dumps(lease)), '123\n']):
            self.assertEqual(m.worker_prepare(layout), lease)
        lease['worker_id'] = 'wrong-worker'
        with patch.object(m, 'run', return_value=subprocess.CompletedProcess([], 0, json.dumps(lease))), patch.object(m, 'worker_abort') as abort:
            with self.assertRaises(m.UpdateError):
                m.worker_prepare(layout)
            abort.assert_called_once_with(layout, 'lease-token')

    def test_worker_stopped_requires_dead_status_pid(self):
        layout = FakeLayout(self.root, 'worker')
        answers = ['MainPID=0\nActiveState=inactive\n', subprocess.CompletedProcess([], 0, '{"pid":123}')]
        with patch.object(m, 'run', side_effect=answers), patch.object(m.os, 'kill', side_effect=ProcessLookupError):
            self.assertFalse(m.worker_running(layout))
        with patch.object(m, 'run', side_effect=answers), patch.object(m.os, 'kill'):
            with self.assertRaises(m.Busy):
                m.worker_running(layout)

    def test_timestamp_normalizes_python310_fractional_lengths(self):
        for fraction in ('1', '12', '123', '1234', '12345', '123456', '123456789'):
            value = m.timestamp('2026-09-16T00:00:00.' + fraction + 'Z')
            self.assertIsInstance(value, float)

    def test_gateway_readiness_requires_active_managed_binary(self):
        layout = FakeLayout(self.root)
        with patch.object(m, 'run', return_value='ActiveState=inactive\nMainPID=0\n'):
            self.assertFalse(m.gateway_service_active(layout))
        with patch.object(m, 'run', return_value='ActiveState=active\nMainPID=123\n'), patch.object(m.os.path, 'samefile', return_value=False):
            self.assertFalse(m.gateway_service_active(layout))
        with patch.object(m, 'run', return_value='ActiveState=active\nMainPID=123\n'), patch.object(m.os.path, 'samefile', return_value=True):
            self.assertTrue(m.gateway_service_active(layout))
        response = types.SimpleNamespace(status=200)
        response_context = unittest.mock.MagicMock()
        response_context.__enter__.return_value = response
        with patch.object(m.urllib.request, 'urlopen', return_value=response_context), patch.object(m, 'gateway_service_active', return_value=False), patch.object(m.time, 'sleep'):
            with self.assertRaises(m.UpdateError):
                m.gateway_ready(layout, timeout=0.01)

    def test_codex_and_node_resolve_from_installing_users_path(self):
        tool_dir = self.root / 'custom-node/bin'
        tool_dir.mkdir(parents=True)
        for name in ('codex', 'node'):
            executable = tool_dir / name
            executable.write_text('#!/bin/sh\nexit 0\n')
            executable.chmod(0o755)
        cfg = {'runtimes': [{'codex_binary': 'codex'}]}
        with patch.dict(os.environ, {'PATH': str(tool_dir)}):
            service_path = m.worker_executable_paths(cfg)
        self.assertEqual(cfg['runtimes'][0]['codex_binary'], str(tool_dir / 'codex'))
        self.assertIn(str(tool_dir), service_path.split(':'))

    def test_fresh_failure_removes_created_configuration_so_install_can_retry(self):
        layout, packages, release = self.update_fixtures('worker')
        layout.config.unlink()
        layout.state.unlink()
        layout.manager.unlink()
        layout.unit = self.root / 'services/codex-worker.service'
        layout.environment = None
        source = self.root / 'source-config.json'
        source.write_bytes(b'original private source')
        def configure(*args):
            layout.config.write_text('{}')
            (layout.config.parent / 'worker.token').write_text('private fixture')
        def service(*args):
            layout.unit.parent.mkdir(exist_ok=True)
            layout.unit.write_text('test service')
        with patch.object(m, 'install_configuration', side_effect=configure), patch.object(m, 'install_service', side_effect=service), patch.object(m, 'apply_update', side_effect=m.UpdateError('migration failed')), patch.object(m, 'run'):
            for attempt in range(2):
                with self.assertRaisesRegex(m.UpdateError, 'migration failed'):
                    m.fresh_install(layout, source, None, packages, release)
                self.assertFalse(layout.config.exists())
                self.assertFalse(layout.unit.exists())
                self.assertFalse((layout.config.parent / 'worker.token').exists())
        self.assertEqual(source.read_bytes(), b'original private source')

    def test_fresh_binary_directory_remains_traversable_with_private_umask(self):
        layout, packages, release = self.update_fixtures()
        layout.bin = self.root / 'new-bin'
        layout.binary = layout.bin / 'codex-gateway'
        layout.command = layout.bin / 'codex-telegramgw'
        previous = os.umask(0o077)
        try:
            with patch.object(m, 'GATEWAY_DATA_ROOT', self.root / 'data'), patch.object(m, 'run'), patch.object(m, 'gateway_ready'):
                m.apply_update(layout, packages, release, fresh=True)
        finally:
            os.umask(previous)
        self.assertEqual(layout.bin.stat().st_mode & 0o777, 0o755)
        self.assertEqual(layout.binary.stat().st_mode & 0o777, 0o755)

    def test_sqlite_backup_includes_wal_commits(self):
        db_path = self.root / 'source.db'
        backup = self.root / 'backup.db'
        with sqlite3.connect(db_path) as source:
            source.execute('PRAGMA journal_mode=WAL')
            source.execute('CREATE TABLE values_table (value TEXT)')
            source.execute("INSERT INTO values_table VALUES ('committed in WAL')")
            source.commit()
            m.sqlite_backup(db_path, backup)
        with sqlite3.connect(backup) as copied:
            self.assertEqual(copied.execute('SELECT value FROM values_table').fetchone()[0], 'committed in WAL')


if __name__ == '__main__':
    unittest.main()
