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
    def test_no_source_refuses(self):
        with settings(), self.assertRaisesRegex(RuntimeError, "set SUBSTRATE_SOURCE in .env"):
            _common.cluster()

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

    def test_ready_line_quotes_paths(self):
        c = local.LocalCluster("/my src", shell=None)
        self.assertEqual(_common.ready(c),
                         "Substrate is ready: KUBECONFIG='/my src/bin/substrate/kubeconfig' "
                         "'/my src/bin/kubectl-ate' get actors")
