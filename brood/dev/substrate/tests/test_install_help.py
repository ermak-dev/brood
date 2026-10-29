"""Справка установки должна называть настоящий источник runtime."""
import unittest

from brood.dev.substrate import local
from brood.dev.substrate.cli import install


class InstallHelpTests(unittest.TestCase):
    # BUG: подкоманда обещает старый upstream, хотя installer закрепляет форк (#74).
    # HYPOTHESIS: при смене pin обновили справку группы, но не install.
    # RESULT: FIXED — install называет тот же credential-free fork URL, что installer.
    def test_install_help_names_the_pinned_fork(self):
        self.assertIn(local.REPOSITORY, install.__doc__)
        self.assertNotIn("clones github.com/agent-substrate/substrate", install.__doc__)
