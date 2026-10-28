"""Командлеты `brood substrate`: группа, настройка checkout'а, отказы."""
import contextlib
import io
import os
import tempfile
import unittest
from unittest import mock

from brood.common import cli
from brood.dev.substrate import local
from brood.dev.substrate.cli import _common, start


def settings(**values):
    return mock.patch.object(_common.config, "get", lambda name: values.get(name, ""))


class GroupTests(unittest.TestCase):
    def test_substrate_is_a_top_level_group(self):
        self.assertEqual(cli.scan()["substrate"], "brood.dev.substrate.cli")
        self.assertEqual(set(cli.groups()["substrate"]), {"install", "start", "stop", "uninstall"})
        self.assertEqual(cli.resolve(["substrate", "start"], cli.groups(), cli.scan()),
                         ("brood.dev.substrate.cli.start", []))

    def test_extra_arguments_refused(self):
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            self.assertEqual(cli.run(start.main, ["now"]), 1)
        self.assertEqual(err.getvalue(), "brood substrate start takes no arguments\n")


class ClusterTests(unittest.TestCase):
    def test_without_source_uses_managed_checkout(self):
        """Без SUBSTRATE_SOURCE -- свой клон; install переводит его на COMMIT."""
        with tempfile.TemporaryDirectory() as tmp, settings():
            managed = os.path.join(tmp, "brood", "substrate")
            synced = []

            def sync(shell, path, repository=local.REPOSITORY, commit=local.COMMIT):
                synced.append((path, repository, commit))
                for marker in local.MARKERS:
                    os.makedirs(os.path.join(path, marker))
                return path
            with mock.patch.object(local, "sync", sync):
                c = _common.cluster({"XDG_DATA_HOME": tmp, "PATH": "/usr/bin"}, fetch=True)
        self.assertEqual(synced, [(managed, local.REPOSITORY, local.COMMIT)])
        self.assertEqual(c.source, managed)

    def test_without_source_others_do_not_fetch(self):
        """stop, start, uninstall не ходят в сеть и не меняют коммит клона."""
        with tempfile.TemporaryDirectory() as tmp, settings():
            with mock.patch.object(local, "sync", side_effect=AssertionError("fetched")):
                with self.assertRaisesRegex(RuntimeError, "not installed: run brood "
                                                          "substrate install"):
                    _common.cluster({"XDG_DATA_HOME": tmp})
                managed = os.path.join(tmp, "brood", "substrate")
                for marker in local.MARKERS:
                    os.makedirs(os.path.join(managed, marker))
                self.assertEqual(_common.cluster({"XDG_DATA_HOME": tmp}).source, managed)

    def test_source_is_never_fetched(self):
        with tempfile.TemporaryDirectory() as tmp, settings(SUBSTRATE_SOURCE=tmp):
            for marker in local.MARKERS:
                os.makedirs(os.path.join(tmp, marker))
            with mock.patch.object(local, "sync", side_effect=AssertionError("fetched")):
                self.assertEqual(_common.cluster({}, fetch=True).source, tmp)

    def test_source_must_be_a_checkout(self):
        with tempfile.TemporaryDirectory() as tmp, settings(SUBSTRATE_SOURCE=tmp):
            with self.assertRaisesRegex(RuntimeError, "is not a Substrate checkout"):
                _common.cluster()

    def test_cluster_of_checkout(self):
        with tempfile.TemporaryDirectory() as tmp, settings(SUBSTRATE_SOURCE=tmp):
            for marker in local.MARKERS:
                os.makedirs(os.path.join(tmp, marker))
            c = _common.cluster({"PATH": "/usr/bin", "ATE_CONFIG": "/x"})
        self.assertEqual(c.kubeconfig, os.path.join(tmp, "bin", "substrate", "kubeconfig"))
        self.assertEqual(c.shell.cwd, tmp)
        self.assertNotIn("ATE_CONFIG", c.shell.env)

    # BUG: Подсказка без -A отклоняется kubectl-ate (#8).
    # HYPOTHESIS: Общему списку акторов требуется явный scope всех atespace.
    # SOLUTION: Подсказка передаёт -A, сохраняя shell quoting путей.
    def test_ready_line_quotes_paths(self):
        c = local.LocalCluster("/my src", shell=None)
        self.assertEqual(_common.ready(c),
                         "Substrate is ready: KUBECONFIG='/my src/bin/substrate/kubeconfig' "
                         "'/my src/bin/kubectl-ate' get actors -A")
