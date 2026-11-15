"""Канонические координаты трекера не меняют Git transport гостевого клона."""
# BUG: Трекер читает loopback origin вместо канонического brood.origin (#302).
# HYPOTHESIS: Координаты API и проверка checkout независимо читают remote origin.
# SOLUTION: Единый resolver читает только локальный brood.origin; transport не меняется.
# RESULT: FIXED — канонические координаты и проверки проекта работают через gateway.
# STATUS: FIXED
import contextlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

from brood.dev import gitlab


class GatewayCoordinatesTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.repo = self.root / "project"
        self.gateway = "http://127.0.0.1:8318/origin.git"
        self.canonical = "https://git.example:8443/group/project.git"
        self.enterContext(mock.patch.dict(os.environ, {
            "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"}))
        subprocess.run(["git", "init", "-q", str(self.repo)], check=True)
        self.git("remote", "add", "origin", self.gateway)
        self.git("config", "--local", "brood.origin", self.canonical)
        self.enterContext(contextlib.chdir(self.repo))
        self.enterContext(mock.patch.object(gitlab.config, "PROJECT", str(self.repo)))
        self.settings = {}
        self.enterContext(mock.patch.object(gitlab.config, "get", self.settings.get))

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.repo), *args],
                              check=True, capture_output=True, text=True).stdout.strip()

    def test_tracker_coordinates_use_canonical_origin(self):
        self.assertEqual(gitlab.origin(), self.canonical)
        self.assertEqual(gitlab.site(), "git.example:8443")
        self.assertEqual(gitlab.project(), "group/project")
        self.assertEqual(gitlab.base(),
                         "https://git.example:8443/api/v4/projects/group%2Fproject")
        self.assertEqual(self.git("remote", "get-url", "origin"), self.gateway)

    def test_checkout_validation_uses_canonical_origin_with_explicit_settings(self):
        self.settings.update(GITLAB_SITE="git.example:8443", GITLAB_PROJECT="group/project")
        try:
            checkout = gitlab.require_local_project()
        except RuntimeError as error:
            self.fail(f"gateway checkout with matching canonical origin was refused: {error}")
        self.assertEqual(checkout, str(self.repo))
        self.assertEqual(self.git("remote", "get-url", "origin"), self.gateway)

    def test_canonical_origin_does_not_authorize_other_project_or_site(self):
        self.settings.update(GITLAB_SITE="git.example:8443", GITLAB_PROJECT="group/project")
        for name, value in (("GITLAB_SITE", "other.example:8443"),
                            ("GITLAB_SITE", "git.example:443"),
                            ("GITLAB_PROJECT", "other/project")):
            with self.subTest(name=name, value=value):
                saved = self.settings[name]
                self.settings[name] = value
                try:
                    with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                        gitlab.require_local_project()
                finally:
                    self.settings[name] = saved
        with gitlab.project_scope("other/project"):
            with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                gitlab.require_local_project()

    def test_transport_match_cannot_override_mismatched_canonical_origin(self):
        self.git("remote", "set-url", "origin", "https://git.example:8443/other/project.git")
        self.settings.update(GITLAB_SITE="git.example:8443", GITLAB_PROJECT="other/project")
        with self.assertRaisesRegex(RuntimeError, "origin does not match"):
            gitlab.require_local_project()

    def test_ordinary_checkout_falls_back_to_remote_origin(self):
        self.git("config", "--local", "--unset", "brood.origin")
        self.git("remote", "set-url", "origin", self.canonical)
        self.assertEqual(gitlab.origin(), self.canonical)
        self.assertEqual(gitlab.site(), "git.example:8443")
        self.assertEqual(gitlab.require_local_project(), str(self.repo))

    def test_each_candidate_checkout_uses_its_own_canonical_origin(self):
        foreign = self.root / "foreign"
        subprocess.run(["git", "init", "-q", str(foreign)], check=True)
        subprocess.run(["git", "-C", str(foreign), "remote", "add", "origin", self.gateway], check=True)
        subprocess.run(["git", "-C", str(foreign), "config", "--local", "brood.origin",
                        "https://git.example:8443/other/project.git"], check=True)
        with contextlib.chdir(foreign):
            self.assertEqual(gitlab.require_local_project(), str(self.repo))
            with gitlab.project_scope("other/project"):
                self.assertEqual(gitlab.require_local_project(), str(foreign))
            with gitlab.project_scope("third/project"):
                with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                    gitlab.require_local_project()

    def test_global_canonical_origin_is_not_checkout_metadata(self):
        self.git("config", "--local", "--unset", "brood.origin")
        self.git("remote", "set-url", "origin", self.canonical)
        global_config = self.root / "global.gitconfig"
        global_config.write_text('[brood]\n\torigin = https://foreign.example/other/project.git\n')
        with mock.patch.dict(os.environ, {"GIT_CONFIG_GLOBAL": str(global_config)}):
            self.assertEqual(gitlab.origin(), self.canonical)
            self.assertEqual(gitlab.require_local_project(), str(self.repo))

    def test_invalid_canonical_origin_does_not_fall_back_to_transport(self):
        self.git("remote", "set-url", "origin", self.canonical)
        self.settings.update(GITLAB_SITE="git.example:8443", GITLAB_PROJECT="group/project")
        for canonical in ("", "/local/repo", "ftp://git.example:8443/group/project.git",
                          "https://git.example:bad/group/project.git"):
            with self.subTest(canonical=canonical):
                self.git("config", "--local", "brood.origin", canonical)
                with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                    gitlab.require_local_project()
