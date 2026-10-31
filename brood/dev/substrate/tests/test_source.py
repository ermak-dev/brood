"""Свой клон Substrate: каталог данных и перевод клона на закреплённый коммит.

git -- настоящий, апстрим -- репозиторий во временном каталоге: проверяется
то, что git сделает с клоном, а не журнал команд.
"""
import os
import shutil
import subprocess
import tempfile
import unittest

from brood.dev.substrate import local

GIT_ENV = {"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t",
           "GIT_COMMITTER_EMAIL": "t@t", "GIT_CONFIG_GLOBAL": os.devnull,
           "GIT_CONFIG_NOSYSTEM": "1"}


class ManagedCheckoutTests(unittest.TestCase):
    def test_data_home(self):
        self.assertEqual(local.managed_checkout({"XDG_DATA_HOME": "/data", "HOME": "/home/u"}),
                         "/data/brood/substrate")

    def test_default_data_home(self):
        for environ in ({"HOME": "/home/u"}, {"HOME": "/home/u", "XDG_DATA_HOME": ""},
                        # Относительный XDG_DATA_HOME спецификация велит пропускать.
                        {"HOME": "/home/u", "XDG_DATA_HOME": "rel"}):
            with self.subTest(environ=environ):
                self.assertEqual(local.managed_checkout(environ),
                                 "/home/u/.local/share/brood/substrate")


class SyncTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.env = {**os.environ, **GIT_ENV}
        self.upstream = os.path.join(tmp.name, "upstream")
        self.url = "file://" + self.upstream
        self.path = os.path.join(tmp.name, "data", "brood", "substrate")
        self.git(None, "init", "-q", "-b", "main", self.upstream)
        self.first = self.commit("one")
        self.second = self.commit("two")
        self.shell = local.Shell(self.env, None)

    def git(self, cwd, *args):
        return subprocess.run(["git", *(["-C", cwd] if cwd else []), *args], env=self.env,
                              capture_output=True, text=True, check=True).stdout.strip()

    def commit(self, text):
        with open(os.path.join(self.upstream, "README"), "w") as f:
            f.write(text + "\n")
        self.git(self.upstream, "add", "README")
        self.git(self.upstream, "commit", "-q", "-m", text)
        return self.git(self.upstream, "rev-parse", "HEAD")

    def sync(self, commit):
        return local.sync(self.shell, self.path, self.url, commit)

    def head(self):
        return self.git(self.path, "rev-parse", "HEAD")

    def test_clones_at_commit(self):
        self.assertEqual(self.sync(self.first), self.path)
        self.assertEqual(self.head(), self.first)
        with open(os.path.join(self.path, "README")) as f:
            self.assertEqual(f.read(), "one\n")

    def test_same_commit_does_not_fetch(self):
        self.sync(self.first)
        shutil.rmtree(self.upstream)
        self.assertEqual(self.sync(self.first), self.path)

    def test_moves_to_new_commit_keeping_state(self):
        """Записи владения лежат в неотслеживаемом bin/substrate клона."""
        self.sync(self.first)
        state = local.state_dir(self.path)
        os.makedirs(state)
        open(os.path.join(state, "owned"), "w").close()
        self.sync(self.second)
        self.assertEqual(self.head(), self.second)
        self.assertTrue(os.path.isfile(os.path.join(state, "owned")))

    def test_foreign_directory_refuses(self):
        os.makedirs(self.path)
        with open(os.path.join(self.path, "mine"), "w") as f:
            f.write("data\n")
        with self.assertRaisesRegex(RuntimeError, "exists and is not a git checkout"):
            self.sync(self.first)
        self.assertEqual(os.listdir(self.path), ["mine"])

    def test_empty_directory_is_used(self):
        os.makedirs(self.path)
        self.sync(self.first)
        self.assertEqual(self.head(), self.first)

    def test_other_origin_refuses(self):
        self.sync(self.first)
        self.git(self.path, "remote", "set-url", "origin", "https://example.com/fork.git")
        with self.assertRaisesRegex(RuntimeError, "origin is https://example.com/fork.git"):
            self.sync(self.second)
        self.assertEqual(self.head(), self.first)

    def test_local_changes_refuse(self):
        self.sync(self.first)
        with open(os.path.join(self.path, "README"), "w") as f:
            f.write("edited\n")
        with self.assertRaisesRegex(RuntimeError, "has local changes"):
            self.sync(self.second)
        self.assertEqual(self.head(), self.first)

    def test_unknown_commit_refuses(self):
        with self.assertRaisesRegex(RuntimeError, "git fetch"):
            self.sync("0" * 40)


class StepTests(unittest.TestCase):
    def test_step_names_git_subcommand(self):
        self.assertEqual(local.step(("git", "-C", "/x", "fetch", "-q", "--depth")),
                         "git fetch -q")
        self.assertEqual(local.step(("/src/hack/kind.sh", "create", "cluster", "--name")),
                         "kind.sh create cluster")


class PinTests(unittest.TestCase):
    def test_verified_lxc_fork_at_full_commit(self):
        self.assertEqual(local.REPOSITORY, "git@git.ermak.dev:ermak/substrate.git")
        self.assertEqual(local.COMMIT, "f0e0a630a2d72a013d9ad97ea71d092bd7c26713")
