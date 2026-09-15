#!/usr/bin/env python3
"""Offline, validated PostgreSQL -> SQLite migration. Uses psql's PG* settings.

Stop every gateway writer before running. The source is read-only and retained.
The destination must not exist; it is published only after complete validation.
No credentials, prompts, event bodies, or row values are written to stdout.
"""
import argparse
import datetime
import decimal
import hashlib
import json
import os
import pathlib
import re
import sqlite3
import subprocess
import sys
import tempfile


NOW = "(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')"


def json_text(value):
    if isinstance(value, dict):
        return '{' + ','.join(json.dumps(k, ensure_ascii=False) + ':' + json_text(v)
                              for k, v in sorted(value.items())) + '}'
    if isinstance(value, (list, tuple)):
        return '[' + ','.join(map(json_text, value)) + ']'
    if isinstance(value, decimal.Decimal):
        if not value.is_finite():
            raise ValueError('non-finite JSON number')
        return str(value)
    return json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(',', ':'))


def timestamp(value):
    # PostgreSQL retains microseconds. Fixed-width UTC strings keep SQLite's
    # comparisons chronological while matching the gateway's nanosecond format.
    value = value.replace('Z', '+00:00')
    value = re.sub(r'\.(\d{1,6})(?=[+-]\d{2}:\d{2}$)',
                   lambda match: '.' + match.group(1).ljust(6, '0'), value)
    parsed = datetime.datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        raise ValueError('source timestamp lacks timezone')
    parsed = parsed.astimezone(datetime.timezone.utc)
    return parsed.strftime('%Y-%m-%dT%H:%M:%S.') + f'{parsed.microsecond:06d}000Z'


def convert(value, kind):
    if value is None:
        return None
    if kind == 'bytea':
        if not isinstance(value, str) or not value.startswith('\\x'):
            raise ValueError('invalid PostgreSQL bytea representation')
        return bytes.fromhex(value[2:])
    if kind in ('json', 'jsonb'):
        if not isinstance(value, str):
            raise ValueError('JSON source column must be exported as text')
        return json_text(json.loads(value, parse_float=decimal.Decimal))
    if kind == 'timestamptz':
        return timestamp(value)
    if kind == 'bool':
        if not isinstance(value, bool):
            raise ValueError('invalid PostgreSQL boolean')
        return int(value)
    if kind not in ('text', 'uuid', 'int2', 'int4', 'int8', 'varchar'):
        raise ValueError('unsupported PostgreSQL column type: ' + kind)
    return value


def row_digest(values):
    safe = [{'blob_hex': v.hex()} if isinstance(v, bytes) else v for v in values]
    return int.from_bytes(hashlib.sha256(json_text(safe).encode()).digest(), 'big')


def initialize(connection, migrations):
    connection.execute('PRAGMA foreign_keys=ON')
    connection.execute('PRAGMA synchronous=FULL')
    connection.execute('CREATE TABLE IF NOT EXISTS schema_migrations ('
                       'version INTEGER NOT NULL PRIMARY KEY, checksum BLOB NOT NULL, '
                       'applied_at TEXT NOT NULL DEFAULT ' + NOW + ') STRICT')
    for path in sorted(migrations.glob('[0-9]*_*.sql')):
        sql = path.read_bytes()
        connection.executescript(sql.decode())
        connection.execute('INSERT INTO schema_migrations(version,checksum) VALUES(?,?)',
                           (int(path.name.split('_')[0]), hashlib.sha256(sql).digest()))
    connection.commit()
    return [r[0] for r in connection.execute("SELECT name FROM sqlite_schema WHERE type='table' "
                                            "AND name NOT LIKE 'sqlite_%' AND name<>'schema_migrations' ORDER BY name")]


def quote_identifier(value):
    if not re.fullmatch(r'[a-z_][a-z0-9_]*', value):
        raise ValueError('invalid database identifier')
    return '"' + value + '"'


def export_sql(tables, schema):
    namespace = quote_identifier(schema)
    statements = ['BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;', "SET LOCAL TIME ZONE 'UTC';",
                  "SET LOCAL bytea_output='hex';",
                  "SELECT json_build_object('kind','ledger','versions',json_agg(json_build_object("
                  "'version',version,'checksum',encode(checksum,'hex')) ORDER BY version)) "
                  f'FROM {namespace}.schema_migrations;']
    for table in tables:
        quoted = quote_identifier(table)
        statements.append("SELECT json_build_object('kind','columns','table','" + table + "','columns',"
                          "json_agg(json_build_object('name',column_name,'type',udt_name) ORDER BY ordinal_position)) "
                          f"FROM information_schema.columns WHERE table_schema='{schema}' AND table_name='{table}';")
        # Cast JSON columns to text inside PostgreSQL. SQL NULL stays null,
        # while the JSON literal null becomes the string "null" and survives.
        statements.append(
            "SELECT format('SELECT json_build_object(''kind'',''row'',''table'',%L,''row'',json_build_object(%s)) FROM %I.%I;', "
            f"'{table}', string_agg(format('%L, %I%s', column_name, column_name, "
            "CASE WHEN udt_name IN ('json','jsonb') THEN '::text' ELSE '' END), ',' ORDER BY ordinal_position), "
            f"'{schema}', '{table}') FROM information_schema.columns "
            f"WHERE table_schema='{schema}' AND table_name='{table}'\n\\gexec")
    statements.extend(['COMMIT;', "SELECT json_build_object('kind','complete');"])
    return '\n'.join(statements) + '\n'


def import_stream(connection, lines, tables, legacy_migrations):
    expected_ledger = {int(p.name.split('_')[0]): hashlib.sha256(p.read_bytes()).hexdigest()
                       for p in legacy_migrations.glob('[0-9]*_*.sql')}
    if not expected_ledger:
        raise ValueError('legacy migration checksums are unavailable')
    columns, counts, digests = {}, dict.fromkeys(tables, 0), dict.fromkeys(tables, 0)
    ledger_ok = complete = False
    connection.execute('BEGIN IMMEDIATE')
    connection.execute('PRAGMA defer_foreign_keys=ON')
    for line in lines:
        record = json.loads(line, parse_float=decimal.Decimal)
        kind = record['kind']
        if kind == 'ledger':
            actual = {r['version']: r['checksum'] for r in record['versions'] or []}
            if actual != expected_ledger or ledger_ok:
                raise ValueError('PostgreSQL schema does not match supported migration checksums')
            ledger_ok = True
        elif kind == 'columns':
            table = record['table']
            if not ledger_ok or table not in counts or table in columns:
                raise ValueError('unexpected source table metadata')
            cols = record['columns'] or []
            target = {r[1] for r in connection.execute('PRAGMA table_info(' + quote_identifier(table) + ')')}
            if {c['name'] for c in cols} != target:
                raise ValueError('source and destination column sets differ: ' + table)
            columns[table] = cols
        elif kind == 'row':
            table = record['table']
            cols = columns[table]
            if set(record['row']) != {c['name'] for c in cols}:
                raise ValueError('source row column set differs')
            values = [convert(record['row'][c['name']], c['type']) for c in cols]
            connection.execute('INSERT INTO ' + quote_identifier(table) + '(' +
                               ','.join(quote_identifier(c['name']) for c in cols) + ') VALUES(' +
                               ','.join('?' for _ in cols) + ')', values)
            counts[table] += 1
            digests[table] = (digests[table] + row_digest(values)) % (1 << 256)
        elif kind == 'complete':
            complete = True
        else:
            raise ValueError('unexpected export record')
    if not complete or not ledger_ok or set(columns) != set(tables):
        raise ValueError('incomplete PostgreSQL export')
    for table, cols in columns.items():
        count = digest = 0
        for row in connection.execute('SELECT ' + ','.join(quote_identifier(c['name']) for c in cols) +
                                      ' FROM ' + quote_identifier(table)):
            count += 1
            digest = (digest + row_digest(row)) % (1 << 256)
        if count != counts[table] or digest != digests[table]:
            raise ValueError('row count or content verification failed: ' + table)
    if connection.execute('PRAGMA foreign_key_check').fetchone() is not None:
        raise ValueError('foreign key verification failed')
    if connection.execute('PRAGMA integrity_check').fetchall() != [('ok',)]:
        raise ValueError('SQLite integrity check failed')
    return counts


def migrate(destination, schema, migrations):
    quote_identifier(schema)
    destination = pathlib.Path(destination).absolute()
    if destination.exists() or destination.is_symlink():
        raise ValueError('destination already exists; refusing to overwrite')
    if sqlite3.sqlite_version_info < (3, 37, 0):
        raise ValueError('Python SQLite 3.37 or later is required')
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix='.' + destination.name + '.import-', dir=destination.parent)
    os.close(descriptor)
    connection = process = None
    try:
        connection = sqlite3.connect(temporary)
        tables = initialize(connection, migrations)
        with tempfile.TemporaryFile(mode='w+t') as errors, tempfile.TemporaryFile(mode='w+t') as commands:
            commands.write(export_sql(tables, schema))
            commands.seek(0)
            process = subprocess.Popen(['psql', '-X', '-q', '-A', '-t', '-w', '-v', 'ON_ERROR_STOP=1'],
                                       stdin=commands, stdout=subprocess.PIPE, stderr=errors, text=True)
            counts = import_stream(connection, process.stdout, tables, migrations / 'postgres')
            process.stdout.close()
            if process.wait() != 0:
                raise ValueError('PostgreSQL export failed; check connection settings and read permissions')
        connection.commit()
        connection.close()
        connection = None
        with open(temporary, 'rb') as output:
            os.fsync(output.fileno())
        # link() fails if the destination appeared during migration; never replace it.
        os.link(temporary, destination)
        os.unlink(temporary)
        directory = os.open(destination.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        return counts
    finally:
        if process and process.poll() is None:
            process.kill()
            process.wait()
        if connection:
            connection.close()
        for suffix in ('', '-journal', '-wal', '-shm'):
            pathlib.Path(temporary + suffix).unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--sqlite', required=True, help='new SQLite database file; must not exist')
    parser.add_argument('--schema', default='public', help='source PostgreSQL schema')
    args = parser.parse_args()
    try:
        counts = migrate(args.sqlite, args.schema, pathlib.Path(__file__).resolve().parent.parent / 'migrations')
    except Exception as error:
        # Database exceptions can contain private source rows. Report safe error
        # classes only; explicit validation errors never contain stored values.
        message = str(error) if isinstance(error, ValueError) else type(error).__name__
        print('Migration failed: ' + message, file=sys.stderr)
        return 1
    print(f'Migrated {sum(counts.values())} rows in {len(counts)} tables; content, foreign keys, and integrity verified.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
