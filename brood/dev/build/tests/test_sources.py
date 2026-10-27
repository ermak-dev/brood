"""Снимок commit не переносит настройки оператора и ссылки наружу."""
import subprocess
import tempfile
import unittest
from pathlib import Path

from brood.dev.build import sources


class GitCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.git("init", "-q", "--template=")
        self.git("config", "user.email", "fixture@example.invalid")
        self.git("config", "user.name", "Fixture")
        self.git("remote", "add", "origin", "https://example.org/project.git")
        (self.repo / ".gitignore").write_text(".env\nsecrets/\n.brood/\n")
        (self.repo / "code.py").write_text("version = 1\n")
        self.commit()

    def git(self, *args):
        result = subprocess.run(["git", "-C", str(self.repo), *args], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def commit(self):
        self.git("add", ".")
        self.git("commit", "-qm", "fixture")
        self.sha = self.git("rev-parse", "HEAD")


class SourceTests(GitCase):
    def test_snapshot_uses_the_commit_and_excludes_local_settings(self):
        (self.repo / "code.py").write_text("dirty\n")
        (self.repo / ".env").write_text("TOKEN=private")
        (self.repo / "untracked").write_text("not part of commit")
        target = self.root / "snapshot"
        report = sources.snapshot(self.repo, self.sha, target)
        self.assertEqual(report["revision"], self.sha)
        self.assertEqual((target / "code.py").read_text(), "version = 1\n")
        self.assertFalse((target / ".env").exists())
        self.assertFalse((target / "untracked").exists())
        self.assertFalse((target / ".git").exists())

    def test_git_metadata_preserves_revision_not_local_config_or_paths(self):
        self.git("config", "credential.helper", "private-helper")
        target = self.root / "snapshot"
        sources.snapshot(self.repo, self.sha, target, git_metadata=True)
        result = subprocess.run(["git", "-C", str(target), "rev-parse", "HEAD"],
                                capture_output=True, text=True)
        self.assertEqual(result.stdout.strip(), self.sha)
        text = (target / ".git/config").read_text()
        self.assertNotIn("private-helper", text)
        self.assertNotIn(str(self.repo), text)
        self.assertFalse((target / ".git/FETCH_HEAD").exists())
        self.assertFalse(any((target / ".git/hooks").glob("*")))

    def test_repeated_git_metadata_is_byte_stable(self):
        first, second = self.root / "first", self.root / "second"
        sources.snapshot(self.repo, self.sha, first, git_metadata=True)
        sources.snapshot(self.repo, self.sha, second, git_metadata=True)
        def contents(root):
            return {str(p.relative_to(root)): p.read_bytes() for p in root.rglob("*") if p.is_file()}
        self.assertEqual(contents(first / ".git"), contents(second / ".git"))
        result = subprocess.run(["git", "-C", str(first), "status", "--porcelain"],
                                capture_output=True, text=True)
        self.assertEqual(result.stdout, "")

    def test_tracked_secret_is_refused_before_export(self):
        (self.repo / ".env").write_text("TOKEN=private")
        self.git("add", "-f", ".env")
        self.git("commit", "-qm", "fixture secret")
        sha = self.git("rev-parse", "HEAD")
        target = self.root / "snapshot"
        with self.assertRaisesRegex(RuntimeError, "secret|settings"):
            sources.snapshot(self.repo, sha, target)
        self.assertFalse(target.exists())

    def test_vendored_dotenv_fixture_is_not_an_installation_setting(self):
        fixture = self.repo / "vendor/dotenv/.env"
        fixture.parent.mkdir(parents=True)
        fixture.write_text("HELLO=world\n")
        self.git("add", "-f", "vendor/dotenv/.env")
        self.commit()
        target = self.root / "snapshot"
        sources.snapshot(self.repo, self.sha, target)
        self.assertEqual((target / "vendor/dotenv/.env").read_text(), "HELLO=world\n")

    def test_escape_symlink_is_refused(self):
        (self.repo / "escape").symlink_to("../private")
        self.commit()
        with self.assertRaisesRegex(RuntimeError, "symlink"):
            sources.snapshot(self.repo, self.sha, self.root / "snapshot")

    def test_credentials_in_origin_are_never_echoed(self):
        self.git("remote", "set-url", "origin", "https://user:private-token@example.org/project.git")
        with self.assertRaises(RuntimeError) as error:
            sources.snapshot(self.repo, self.sha, self.root / "snapshot", git_metadata=True)
        self.assertNotIn("private-token", str(error.exception))

    def test_existing_destination_is_not_changed(self):
        target = self.root / "snapshot"
        target.mkdir()
        (target / "mine").write_text("keep")
        with self.assertRaisesRegex(RuntimeError, "exists"):
            sources.snapshot(self.repo, self.sha, target)
        self.assertEqual((target / "mine").read_text(), "keep")
