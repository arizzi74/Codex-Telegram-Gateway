import importlib.util
import pathlib
import shutil
import unittest

spec = importlib.util.spec_from_file_location('deployment', pathlib.Path(__file__).with_name('deploy-sqlite-host-update.py'))
deployment = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deployment)


class DeploymentConnectionTests(unittest.TestCase):
    def test_peer_connection_uri_becomes_libpq_environment(self):
        env = deployment.postgres_environment('postgres:///gateway?host=/var/run/postgresql&user=service')
        self.assertEqual(env['PGDATABASE'], 'gateway')
        self.assertEqual(env['PGUSER'], 'service')
        self.assertEqual(env['PGHOST'], '/var/run/postgresql')
        self.assertIsNotNone(shutil.which('runuser', path=env['PATH']))

    def test_uri_credentials_are_decoded_without_putting_uri_in_database_name(self):
        env = deployment.postgres_environment('postgresql://reader:fixture%20value%40host@localhost:5433/gateway%20test?sslmode=require')
        self.assertEqual(env['PGDATABASE'], 'gateway test')
        self.assertEqual(env['PGUSER'], 'reader')
        self.assertEqual(env['PGPASSWORD'], 'fixture value@host')
        self.assertEqual(env['PGPORT'], '5433')
        self.assertEqual(env['PGSSLMODE'], 'require')


if __name__ == '__main__':
    unittest.main()
