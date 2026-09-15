"""Tests for the offline import boundary, without requiring PostgreSQL."""
import hashlib
import decimal
import importlib.util
import json
import pathlib
import sqlite3
import tempfile
import unittest
import uuid
from unittest import mock

ROOT = pathlib.Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('sqlite_migration', ROOT / 'scripts/migrate-postgres-to-sqlite.py')
migration = importlib.util.module_from_spec(spec)
spec.loader.exec_module(migration)


class MigrationTests(unittest.TestCase):
    def test_conversion_keeps_large_json_numbers_blobs_and_time(self):
        from decimal import Decimal
        value = {'integer': 9007199254740993, 'decimal': Decimal('12345678901234567890.123456789'), 'text': '<é>'}
        encoded = migration.convert(migration.json_text(value), 'jsonb')
        self.assertEqual(json.loads(encoded, parse_float=Decimal), value)
        self.assertEqual(migration.convert('\\x0000ff10', 'bytea'), b'\x00\x00\xff\x10')
        self.assertEqual(migration.convert('2026-09-15T12:30:00.123456+02:00', 'timestamptz'),
                         '2026-09-15T10:30:00.123456000Z')
        self.assertEqual(migration.convert('2026-09-14T04:37:31.80991+00:00', 'timestamptz'),
                         '2026-09-14T04:37:31.809910000Z')
        self.assertEqual(migration.convert(False, 'bool'), 0)
        self.assertIsNone(migration.convert(None, 'text'))
        self.assertIsNone(migration.convert(None, 'jsonb'))
        self.assertEqual(migration.convert('null', 'jsonb'), 'null')

    def test_failure_does_not_publish_destination_or_keep_staging_file(self):
        with tempfile.TemporaryDirectory() as directory:
            destination = pathlib.Path(directory) / 'gateway.db'
            with mock.patch.object(migration.subprocess, 'Popen', side_effect=OSError('fixture')):
                with self.assertRaises(OSError):
                    migration.migrate(destination, 'public', ROOT / 'migrations')
            self.assertFalse(destination.exists())
            self.assertEqual(list(pathlib.Path(directory).iterdir()), [])

    def test_refuses_existing_destination_without_touching_it(self):
        with tempfile.TemporaryDirectory() as directory:
            destination = pathlib.Path(directory) / 'gateway.db'
            destination.write_bytes(b'preserve existing state')
            with self.assertRaisesRegex(ValueError, 'already exists'):
                migration.migrate(destination, 'public', ROOT / 'migrations')
            self.assertEqual(destination.read_bytes(), b'preserve existing state')

    def fixture(self, connection):
        tables = migration.initialize(connection, ROOT / 'migrations')
        versions = [{'version': int(p.name.split('_')[0]), 'checksum': hashlib.sha256(p.read_bytes()).hexdigest()}
                    for p in sorted((ROOT / 'migrations/postgres').glob('[0-9]*_*.sql'))]
        yield json.dumps({'kind': 'ledger', 'versions': versions})
        # Empty export still verifies every table and the exact column topology.
        for table in tables:
            columns = [{'name': row[1], 'type': 'text'}
                       for row in connection.execute('PRAGMA table_info(' + migration.quote_identifier(table) + ')')]
            yield json.dumps({'kind': 'columns', 'table': table, 'columns': columns})
        yield json.dumps({'kind': 'complete'})

    def test_complete_empty_snapshot_checks_all_tables(self):
        connection = sqlite3.connect(':memory:')
        records = list(self.fixture(connection))
        tables = [json.loads(line)['table'] for line in records if json.loads(line)['kind'] == 'columns']
        counts = migration.import_stream(connection, records, tables, ROOT / 'migrations/postgres')
        self.assertGreater(len(counts), 10)
        self.assertEqual(sum(counts.values()), 0)
        connection.commit()
        connection.close()

    def test_populated_snapshot_preserves_nulls_types_and_deferred_foreign_keys(self):
        connection = sqlite3.connect(':memory:')
        self.addCleanup(connection.close)
        connection.execute('PRAGMA foreign_keys=ON')
        connection.executescript('''
            CREATE TABLE parents (
                id TEXT NOT NULL PRIMARY KEY,
                required_json TEXT NOT NULL CHECK (json_valid(required_json)),
                metadata TEXT NOT NULL CHECK (json_valid(metadata)),
                opaque BLOB NOT NULL,
                occurred_at TEXT NOT NULL,
                enabled INTEGER NOT NULL
            ) STRICT;
            CREATE TABLE children (
                id INTEGER NOT NULL PRIMARY KEY,
                parent_id TEXT NOT NULL REFERENCES parents(id),
                nullable_json TEXT CHECK (nullable_json IS NULL OR json_valid(nullable_json))
            ) STRICT;
        ''')
        versions = [{'version': int(p.name.split('_')[0]), 'checksum': hashlib.sha256(p.read_bytes()).hexdigest()}
                    for p in sorted((ROOT / 'migrations/postgres').glob('[0-9]*_*.sql'))]
        exact = {'integer': 9007199254740993,
                 'decimal': decimal.Decimal('123456789012345678901234567890.1234567890123456789'),
                 'tiny': decimal.Decimal('0.00000000000000000000000000123'),
                 'text': '<é> \\ "\n', 'null': None}
        # Alphabetical export order puts these child rows before their parent.
        # JSON is exported as text: "null" differs from a SQL NULL value.
        records = [
            {'kind': 'ledger', 'versions': versions},
            {'kind': 'columns', 'table': 'children', 'columns': [
                {'name': 'id', 'type': 'int8'}, {'name': 'parent_id', 'type': 'text'},
                {'name': 'nullable_json', 'type': 'jsonb'}]},
            {'kind': 'row', 'table': 'children', 'row': {
                'id': 1, 'parent_id': 'parent', 'nullable_json': 'null'}},
            {'kind': 'row', 'table': 'children', 'row': {
                'id': 2, 'parent_id': 'parent', 'nullable_json': None}},
            {'kind': 'columns', 'table': 'parents', 'columns': [
                {'name': 'id', 'type': 'text'}, {'name': 'required_json', 'type': 'jsonb'},
                {'name': 'metadata', 'type': 'jsonb'}, {'name': 'opaque', 'type': 'bytea'},
                {'name': 'occurred_at', 'type': 'timestamptz'}, {'name': 'enabled', 'type': 'bool'}]},
            {'kind': 'row', 'table': 'parents', 'row': {
                'id': 'parent', 'required_json': 'null', 'metadata': migration.json_text(exact),
                'opaque': '\\x0000ff10', 'occurred_at': '2026-09-15T12:34:56.123456+02:00',
                'enabled': False}},
            {'kind': 'complete'},
        ]
        counts = migration.import_stream(connection, map(json.dumps, records),
                                         ['children', 'parents'], ROOT / 'migrations/postgres')
        connection.commit()
        self.assertEqual(counts, {'children': 2, 'parents': 1})
        self.assertEqual(connection.execute('SELECT nullable_json,typeof(nullable_json) FROM children ORDER BY id').fetchall(),
                         [('null', 'text'), (None, 'null')])
        parent = connection.execute('SELECT required_json,metadata,opaque,occurred_at,enabled FROM parents').fetchone()
        self.assertEqual(parent[0], 'null')
        self.assertEqual(json.loads(parent[1], parse_float=decimal.Decimal), exact)
        self.assertEqual(parent[2:], (b'\x00\x00\xff\x10', '2026-09-15T10:34:56.123456000Z', 0))
        self.assertEqual(connection.execute('PRAGMA foreign_key_check').fetchall(), [])

    def test_populated_snapshot_rejects_orphaned_foreign_key(self):
        connection = sqlite3.connect(':memory:')
        self.addCleanup(connection.close)
        connection.execute('PRAGMA foreign_keys=ON')
        connection.executescript('''
            CREATE TABLE parents (id INTEGER PRIMARY KEY);
            CREATE TABLE children (parent_id INTEGER NOT NULL REFERENCES parents(id));
        ''')
        versions = [{'version': int(p.name.split('_')[0]), 'checksum': hashlib.sha256(p.read_bytes()).hexdigest()}
                    for p in sorted((ROOT / 'migrations/postgres').glob('[0-9]*_*.sql'))]
        records = [
            {'kind': 'ledger', 'versions': versions},
            {'kind': 'columns', 'table': 'children', 'columns': [{'name': 'parent_id', 'type': 'int8'}]},
            {'kind': 'row', 'table': 'children', 'row': {'parent_id': 99}},
            {'kind': 'columns', 'table': 'parents', 'columns': [{'name': 'id', 'type': 'int8'}]},
            {'kind': 'complete'},
        ]
        with self.assertRaisesRegex(ValueError, 'foreign key verification failed'):
            migration.import_stream(connection, map(json.dumps, records),
                                    ['children', 'parents'], ROOT / 'migrations/postgres')
        connection.rollback()
        self.assertEqual(connection.execute('SELECT count(*) FROM children').fetchone(), (0,))

    def test_gateway_schema_accepts_sql_null_and_json_null_approval_resolution(self):
        # Python's supported SQLite 3.37 returns 0 for json_valid(NULL), while
        # newer versions return NULL. The real schema must work with both.
        connection = sqlite3.connect(':memory:')
        self.addCleanup(connection.close)
        migration.initialize(connection, ROOT / 'migrations')
        worker, runtime, session = (str(uuid.uuid4()) for _ in range(3))
        connection.execute('INSERT INTO workers(worker_id,name,os,arch,auth_token_hash) VALUES(?,?,?,?,?)',
                           (worker, 'fixture', 'linux', 'amd64', b'\x01' * 32))
        connection.execute('INSERT INTO runtimes(runtime_id,worker_id,profile_id,name,state) VALUES(?,?,?,?,?)',
                           (runtime, worker, 'main', 'fixture', 'running'))
        connection.execute('INSERT INTO sessions(session_id,worker_id,runtime_id,codex_thread_id,state) VALUES(?,?,?,?,?)',
                           (session, worker, runtime, 'thread', 'idle'))
        for index, resolution in enumerate((None, 'null')):
            connection.execute('''INSERT INTO approvals(approval_id,worker_id,runtime_id,runtime_generation,
                               session_id,codex_request_id,codex_thread_id,approval_type,request_payload,
                               state,requested_at,resolution) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)''',
                               (str(uuid.uuid4()), worker, runtime, 0, session, str(index), 'thread', 'command',
                                '{}', 'pending', '2026-09-15T10:34:56.123456000Z', resolution))
        connection.commit()
        self.assertEqual(connection.execute('SELECT resolution,typeof(resolution) FROM approvals ORDER BY codex_request_id').fetchall(),
                         [(None, 'null'), ('null', 'text')])
        self.assertEqual(connection.execute('PRAGMA foreign_key_check').fetchall(), [])

    def test_truncated_export_cannot_commit(self):
        connection = sqlite3.connect(':memory:')
        records = list(self.fixture(connection))
        tables = [json.loads(line)['table'] for line in records if json.loads(line)['kind'] == 'columns']
        with self.assertRaisesRegex(ValueError, 'incomplete'):
            migration.import_stream(connection, records[:-1], tables, ROOT / 'migrations/postgres')
        connection.rollback()
        connection.close()

    def test_source_schema_mismatch_is_rejected(self):
        connection = sqlite3.connect(':memory:')
        records = list(self.fixture(connection))
        with self.assertRaisesRegex(ValueError, 'checksums'):
            migration.import_stream(connection, [json.dumps({'kind': 'ledger', 'versions': []})], [], ROOT / 'migrations/postgres')
        connection.rollback()
        connection.close()


if __name__ == '__main__':
    unittest.main()
