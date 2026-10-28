"""Командлет LXC требует явного trusted-only согласия и делегирует библиотеке."""
import contextlib
import io
import unittest
from unittest.mock import Mock, patch

from brood.dev.deploy.cli import lxc as command


# FEATURE: Явный brood deploy lxc, без скрытого переключения runtime (#76).
# SPEC: Без --trusted-workloads нет даже cluster lookup; help не меняет состояние,
# успех печатает точный digest и ограничения, библиотека вызывается напрямую.
class LXCCLITests(unittest.TestCase):
    def test_missing_acknowledgement_or_extra_arguments_do_not_touch_installation(self):
        for args in [[], ["--force"], ["--trusted-workloads", "other"],
                     ["--trusted-workloads", "--trusted-workloads"]]:
            with self.subTest(args=args), patch.object(command, "cluster") as cluster:
                with self.assertRaisesRegex(RuntimeError, "--trusted-workloads"):
                    command.main(args)
                cluster.assert_not_called()

    def test_help_is_english_and_has_no_runtime_side_effects(self):
        with patch.object(command, "cluster") as cluster, contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(command.main(["--help"]), 0)
        cluster.assert_not_called()
        for text in ["--trusted-workloads", "shared node kernel", "memory checkpoint", "worker pools"]:
            self.assertIn(text, output.getvalue())

    def test_prepares_explicitly_without_calling_other_commandlets(self):
        image = "localhost:5002/ateworker-lxc@sha256:" + "d" * 64
        install = Mock()
        deployment = Mock()
        deployment.prepare.return_value = image
        with patch.object(command, "cluster", return_value=install), \
                patch.object(command.lxc, "Deploy", return_value=deployment) as factory, \
                contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(command.main(["--trusted-workloads"]), 0)
        factory.assert_called_once_with(install)
        deployment.prepare.assert_called_once_with()
        self.assertIn(image, output.getvalue())
        self.assertIn("no worker pools or actors were changed", output.getvalue())
        self.assertIn("no memory checkpoint", output.getvalue())
