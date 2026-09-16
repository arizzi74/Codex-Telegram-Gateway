#!/usr/bin/env python3
"""Exercise installer verification and the release bundle contract without a network."""
import hashlib
import io
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import types
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parent.parent


def bootstrap_module():
    source = (ROOT / "scripts/install.sh").read_text().split("<<'PY'\n", 1)[1].rsplit("\nPY", 1)[0]
    module = types.ModuleType("release_bootstrap_test")
    exec(compile(source, str(ROOT / "scripts/install.sh"), "exec"), module.__dict__)
    return module


class BootstrapTests(unittest.TestCase):
    def setUp(self):
        self.bootstrap = bootstrap_module()

    def release(self, tag="v0.4.0", **fields):
        return json.dumps(dict(tag_name=tag, draft=False, prerelease=False, **fields)).encode()

    def test_verified_manager_receives_original_arguments_and_pinned_release(self):
        code = b"# verified installer\n"
        manifest = f"{hashlib.sha256(code).hexdigest()}  ./codex-telegramgw-manager.py\n".encode()
        arguments = ["install", "worker", "--config", "/tmp/a config.json"]

        def execute(command, env):
            self.assertEqual(command[2:], arguments)
            self.assertEqual(Path(command[1]).read_bytes(), code)
            self.assertEqual(env["CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE"], "v0.4.0")
            self.installer = Path(command[1])
            return 17

        with mock.patch.object(self.bootstrap, "fetch", side_effect=[self.release(), manifest, code]) as fetch:
            with mock.patch.object(self.bootstrap.subprocess, "call", side_effect=execute):
                self.assertEqual(self.bootstrap.main(arguments), 17)
        self.assertFalse(self.installer.exists())
        self.assertTrue(fetch.call_args_list[0].args[0].endswith("/releases/latest"))
        for call in fetch.call_args_list[1:]:
            self.assertIn("/releases/download/v0.4.0/", call.args[0])

    def test_corrupt_manager_is_never_executed(self):
        manifest = ("0" * 64 + "  codex-telegramgw-manager.py\n").encode()
        with mock.patch.object(self.bootstrap, "fetch", side_effect=[self.release(), manifest, b"bad code"]):
            with mock.patch.object(self.bootstrap.subprocess, "call") as execute:
                with self.assertRaisesRegex(ValueError, "checksum verification failed"):
                    self.bootstrap.main(["install", "gateway"])
                execute.assert_not_called()

    def test_explicit_version_selects_tag_endpoint(self):
        for arguments in (["--version", "v1.2.3"], ["--version=v1.2.3"]):
            with self.subTest(arguments=arguments):
                with mock.patch.object(self.bootstrap, "fetch", return_value=self.release("v1.2.3")) as fetch:
                    self.assertEqual(self.bootstrap.selected_tag(arguments), "v1.2.3")
                    self.assertTrue(fetch.call_args.args[0].endswith("/releases/tags/v1.2.3"))

    def test_unstable_and_conflicting_versions_rejected_before_download(self):
        for arguments in (["--version", "v1.2.3-rc1"], ["--version", "../bad"], ["--version"],
                          ["--version=v1.2.3", "--version=v2.0.0"]):
            with self.subTest(arguments=arguments):
                with mock.patch.object(self.bootstrap, "fetch") as fetch:
                    with self.assertRaises(ValueError):
                        self.bootstrap.selected_tag(arguments)
                    fetch.assert_not_called()

    def test_release_metadata_cannot_override_requested_tag(self):
        metadata = [dict(tag_name="v1.2.4", draft=False, prerelease=False),
                    dict(tag_name="v1.2.3", draft=True, prerelease=False),
                    dict(tag_name="v1.2.3", draft=False, prerelease=True),
                    dict(tag_name="v1.2.3"), dict(tag_name="v1.2.3-rc1", draft=False, prerelease=False)]
        for fields in metadata:
            with self.subTest(metadata=fields):
                with mock.patch.object(self.bootstrap, "fetch", return_value=json.dumps(fields).encode()):
                    with self.assertRaises(ValueError):
                        self.bootstrap.selected_tag(["--version=v1.2.3"])

    def test_manifest_duplicate_missing_or_unsafe_name_is_rejected(self):
        valid = "0" * 64 + "  codex-telegramgw-manager.py\n"
        for manifest in (valid + valid, "0" * 64 + "  ../codex-telegramgw-manager.py\n", "", "bad\n"):
            with self.subTest(manifest=manifest):
                with self.assertRaises(ValueError):
                    self.bootstrap.manager_digest(manifest.encode())

    def test_plain_http_and_https_downgrade_rejected(self):
        with self.assertRaisesRegex(ValueError, "HTTPS"):
            self.bootstrap.fetch("http://github.com/untrusted", 10)
        with self.assertRaisesRegex(ValueError, "HTTPS"):
            self.bootstrap.HTTPSRedirectHandler().redirect_request(None, None, 302, "", {}, "http://github.com/untrusted")


class BundleVerificationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="telegramgw-release-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.dist = self.root / "dist"
        self.dist.mkdir()
        (self.root / "scripts").mkdir()
        shutil.copy(ROOT / "scripts/verify-release.sh", self.root / "scripts/verify-release.sh")
        self.packaged_sources = ("scripts/release-manager.py", "scripts/install-worker.sh", "scripts/migrate-postgres-to-sqlite.py",
                                 "deploy/systemd/codex-worker.service", "deploy/systemd/codex-gateway.service")
        for source in self.packaged_sources + ("scripts/install.sh",):
            target = self.root / source
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(f"source for {source}\n")
        for system in ("linux", "darwin"):
            for arch in ("amd64", "arm64"):
                for binary in ("codex-gateway", "codex-worker", "codex-local"):
                    if binary == "codex-gateway" and system != "linux":
                        continue
                    path = self.dist / f"{binary}-{system}-{arch}.tar.gz"
                    files = {source: (self.root / source).read_bytes() for source in self.packaged_sources}
                    files[binary] = b"test binary\n"
                    files["SHA256SUMS"] = self.checksums({key: files[key] for key in (binary, "scripts/release-manager.py", "scripts/install-worker.sh")})
                    self.archive(path, files, binary)
        shutil.copy(self.root / "scripts/release-manager.py", self.dist / "codex-telegramgw-manager.py")
        shutil.copy(self.root / "scripts/install.sh", self.dist / "install.sh")
        self.write_manifest()

    @staticmethod
    def checksums(files):
        return "".join(f"{hashlib.sha256(data).hexdigest()}  ./{name}\n" for name, data in files.items()).encode()

    @staticmethod
    def archive(path, files, binary, ownership=None):
        with tarfile.open(path, "w:gz") as archive:
            for name, data in files.items():
                entry = tarfile.TarInfo("./" + name)
                entry.mode = 0o755 if name == binary else 0o644
                entry.size = len(data)
                for field, value in (ownership or {}).items():
                    setattr(entry, field, value)
                archive.addfile(entry, io.BytesIO(data))

    def write_manifest(self):
        files = {path.name: path.read_bytes() for path in self.dist.iterdir() if path.name != "SHA256SUMS"}
        (self.dist / "SHA256SUMS").write_bytes(self.checksums(files))

    def verify(self):
        return subprocess.run(["bash", str(self.root / "scripts/verify-release.sh")], capture_output=True, text=True)

    def test_complete_release_is_accepted(self):
        result = self.verify()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_corruption_and_missing_archive_are_rejected(self):
        archive = self.dist / "codex-worker-linux-amd64.tar.gz"
        archive.write_bytes(b"corrupted")
        result = self.verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum mismatch", result.stderr)
        archive.unlink()
        self.write_manifest()
        result = self.verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("exactly ten archives", result.stderr)

    def test_valid_checksums_cannot_hide_stale_manager_source(self):
        (self.dist / "codex-telegramgw-manager.py").write_bytes(b"stale manager")
        self.write_manifest()
        result = self.verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("differs from source", result.stderr)

    def test_valid_outer_checksum_cannot_hide_unsafe_archive_member(self):
        archive = self.dist / "codex-worker-linux-amd64.tar.gz"
        self.archive(archive, {"../outside": b"unsafe"}, "codex-worker")
        self.write_manifest()
        result = self.verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsafe archive member", result.stderr)

    def test_valid_checksums_cannot_hide_build_account_metadata(self):
        path = self.dist / "codex-worker-linux-amd64.tar.gz"
        with tarfile.open(path, "r:gz") as archive:
            files = {member.name.removeprefix("./"): archive.extractfile(member).read() for member in archive.getmembers()}
        for ownership in ({"uid": 1234}, {"gid": 2345}, {"uname": "fictional-builder"}, {"gname": "fictional-build-team"}):
            with self.subTest(ownership=ownership):
                self.archive(path, files, "codex-worker", ownership)
                self.write_manifest()
                result = self.verify()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("build-account ownership metadata", result.stderr)

    def test_release_packager_anonymizes_owners_and_preserves_files_and_modes(self):
        source = (ROOT / "scripts/release.sh").read_text().split("<<'PY'\n", 1)[1].split("\nPY", 1)[0]
        stage = self.root / "package-stage"
        (stage / "nested").mkdir(parents=True)
        (stage / "program").write_bytes(b"executable payload")
        (stage / "program").chmod(0o751)
        (stage / "nested/config").write_bytes(b"configuration payload")
        (stage / "nested/config").chmod(0o640)
        output = self.root / "package.tar.gz"
        subprocess.run(["python3", "-", str(stage), str(output)], input=source, text=True, check=True)
        with tarfile.open(output, "r:gz") as archive:
            members = {member.name.removeprefix("./"): member for member in archive.getmembers()}
            self.assertEqual(set(members), {".", "program", "nested", "nested/config"})
            for member in members.values():
                self.assertEqual((member.uid, member.gid, member.uname, member.gname), (0, 0, "root", "root"))
            self.assertEqual(members["program"].mode, 0o751)
            self.assertEqual(members["nested/config"].mode, 0o640)
            self.assertEqual(archive.extractfile(members["program"]).read(), b"executable payload")
            self.assertEqual(archive.extractfile(members["nested/config"]).read(), b"configuration payload")


if __name__ == "__main__":
    unittest.main()
