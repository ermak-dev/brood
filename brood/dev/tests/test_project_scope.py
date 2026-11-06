# FEATURE: Explicit GitLab project scope for tracker and CI commands.
# SPEC: Explicit project overrides env and origin, stays scoped, and rejects invalid paths.
# IMPLEMENTATION: Context-local overrides are applied once before command dispatch.
# RESULT: IMPLEMENTED — project selection restores nested and concurrent contexts.
# STATUS: DONE
import concurrent.futures
import contextlib
import threading
import io
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

# BUG: Origin validation and issue git operations can use different checkouts.
# HYPOTHESIS: Git commands inherit cwd while origin uses config.PROJECT.
# SOLUTION: Route issue git operations to the validated checkout.
# RESULT: FIXED — validation and git operations use the same checkout.
# STATUS: FIXED
from unittest import mock

from brood.common.cli import main
from brood.dev import gitlab
from brood.dev.cli import dispatch


class ProjectScopeTests(unittest.TestCase):
    # BUG: Unsupported origin schemes and malformed SSH ports can pass validation.
    # HYPOTHESIS: git is treated as SSH and SSH ports are never parsed.
    # SOLUTION: Accept only supported schemes and validate URL ports.
    # RESULT: FIXED — unsupported origins cannot reach tracker writes.
    # STATUS: FIXED
    def test_origin_transport_validation(self):
        from brood.dev.cli.bug import evidence, start

        with tempfile.TemporaryDirectory() as temp:
            subprocess.run(["git", "init", "-q", temp], check=True)
            subprocess.run(["git", "-C", temp, "remote", "add", "origin",
                            "https://git.example/group/project.git"], check=True)
            previous = os.getcwd()
            try:
                os.chdir(temp)
                with mock.patch.object(gitlab.config, "PROJECT", temp), \
                        mock.patch.object(gitlab.config, "get", side_effect=lambda key: {
                            "GITLAB_SITE": "git.example", "GITLAB_PROJECT": "group/project"}.get(key)), \
                        mock.patch.object(gitlab, "issue") as issue, \
                        mock.patch.object(gitlab, "relabel") as relabel, \
                        mock.patch.object(gitlab, "comment") as comment:
                    for url in ("git://git.example:9418/group/project.git",
                                "ftp://git.example/group/project.git",
                                "file://git.example/group/project.git",
                                "ssh://git@git.example:/group/project.git",
                                "https://git.example:/group/project.git",
                                "https://git.example:0/group/project.git",
                                "ssh://git@git.example:bad/group/project.git",
                                "ssh://git@git.example:99999/group/project.git",
                                "https://git.example:bad/group/project.git"):
                        with self.subTest(url=url):
                            subprocess.run(["git", "-C", temp, "remote", "set-url", "origin", url], check=True)
                            with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                                gitlab.require_local_project()
                            for command in (start.main, evidence.main):
                                with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                                    command(["174"])
                    issue.assert_not_called()
                    relabel.assert_not_called()
                    comment.assert_not_called()
                    for url in ("https://git.example/group/project.git",
                                "https://git.example:443/group/project.git",
                                "ssh://git@git.example:2222/group/project.git",
                                "git@git.example:group/project.git"):
                        with self.subTest(url=url):
                            subprocess.run(["git", "-C", temp, "remote", "set-url", "origin", url], check=True)
                            self.assertEqual(gitlab.require_local_project(), temp)
            finally:
                os.chdir(previous)

    # BUG: Checkout selection loses web ports and accepts mismatched origins.
    # HYPOTHESIS: Site derivation drops ports and selection validates only cwd.
    # SOLUTION: Preserve web ports and select a checkout by matching coordinates.
    # RESULT: FIXED — mismatched checkouts are rejected or fall back safely.
    # STATUS: FIXED
    def test_origin_site_and_checkout_selection(self):
        with tempfile.TemporaryDirectory() as temp:
            home, other = (Path(temp) / name for name in ("home", "other"))
            for repo, url in ((home, "https://git.example:8443/group/project.git"),
                              (other, "https://git.example/other/project.git")):
                subprocess.run(["git", "init", "-q", str(repo)], check=True)
                subprocess.run(["git", "-C", str(repo), "remote", "add", "origin", url], check=True)
            previous = os.getcwd()
            try:
                os.chdir(other)
                with mock.patch.object(gitlab.config, "PROJECT", str(home)), \
                        mock.patch.object(gitlab.config, "get", return_value=None):
                    self.assertEqual(gitlab.site(), "git.example:8443")
                    self.assertEqual(Path(gitlab.require_local_project()), home)
                    with gitlab.project_scope("foreign/project"):
                        with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                            gitlab.require_local_project()
                os.chdir(home)
                subprocess.run(["git", "-C", str(home), "remote", "set-url", "origin",
                                "http://git.example/group/project.git"], check=True)
                with mock.patch.object(gitlab.config, "PROJECT", str(home)), \
                        mock.patch.object(gitlab.config, "get", return_value=None):
                    with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                        gitlab.require_local_project()
                subprocess.run(["git", "-C", str(home), "remote", "set-url", "origin",
                                "ssh://git@git.example:2222/group/project.git"], check=True)
                with mock.patch.object(gitlab.config, "PROJECT", str(home)), \
                        mock.patch.object(gitlab.config, "get", return_value=None):
                    self.assertEqual(gitlab.site(), "git.example")
                    self.assertEqual(Path(gitlab.require_local_project()), home)
                with mock.patch.object(gitlab.config, "PROJECT", str(home)), \
                        mock.patch.object(gitlab.config, "get", side_effect=lambda key: {
                            "GITLAB_SITE": "git.example:8443"}.get(key)):
                    with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                        gitlab.require_local_project()
                subprocess.run(["git", "-C", str(home), "remote", "set-url", "origin",
                                "http://git.example:443/group/project.git"], check=True)
                with mock.patch.object(gitlab.config, "PROJECT", str(home)), \
                        mock.patch.object(gitlab.config, "get", return_value=None):
                    with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                        gitlab.require_local_project()
            finally:
                os.chdir(previous)

    # BUG: A matching linked worktree is ignored for the default project.
    # HYPOTHESIS: Checkout selection only considers explicit foreign scopes.
    # SOLUTION: Select the current worktree and validate its coordinates.
    # RESULT: FIXED — matching linked worktrees are used without changing the configured checkout.
    # STATUS: FIXED
    def test_default_project_prefers_linked_worktree(self):
        with tempfile.TemporaryDirectory() as temp:
            repo, linked = (Path(temp) / name for name in ("repo", "linked"))
            subprocess.run(["git", "init", "-q", "-b", "main", str(repo)], check=True)
            for key, value in (("user.name", "Test"), ("user.email", "test@example.com")):
                subprocess.run(["git", "-C", str(repo), "config", key, value], check=True)
            subprocess.run(["git", "-C", str(repo), "commit", "-q", "--allow-empty", "-m", "initial"], check=True)
            subprocess.run(["git", "-C", str(repo), "remote", "add", "origin", "https://git.example/group/project.git"], check=True)
            subprocess.run(["git", "-C", str(repo), "worktree", "add", "-q", "-b", "linked", str(linked)], check=True)
            original = os.getcwd()
            try:
                os.chdir(linked)
                with mock.patch.object(gitlab.config, "PROJECT", str(repo)), mock.patch.object(gitlab.config, "get", return_value=None):
                    self.assertEqual(Path(gitlab.require_local_project()), linked)
                    with gitlab.project_scope("group/project"):
                        self.assertEqual(Path(gitlab.require_local_project()), linked)
            finally:
                os.chdir(original)

    def test_checkout_site_port_environment_and_outside(self):
        from brood.dev.cli.bug import start, evidence

        with tempfile.TemporaryDirectory() as temp:
            home, foreign, outside = (Path(temp) / name for name in ("home", "foreign", "outside"))
            for root, url in ((home, "https://git.example:8443/home/repo.git"),
                              (foreign, "https://git.example:8443/other/project.git")):
                subprocess.run(["git", "init", "-q", "-b", "main", str(root)], check=True)
                for key, value in (("user.name", "Test"), ("user.email", "test@example.com")):
                    subprocess.run(["git", "-C", str(root), "config", key, value], check=True)
                subprocess.run(["git", "-C", str(root), "commit", "-q", "--allow-empty", "-m", "initial"], check=True)
                subprocess.run(["git", "-C", str(root), "remote", "add", "origin", url], check=True)
            outside.mkdir()
            original = os.getcwd()
            try:
                with mock.patch.object(gitlab.config, "PROJECT", str(home)):
                    os.chdir(foreign)
                    with mock.patch.object(gitlab.config, "get", side_effect=lambda k: {"GITLAB_SITE": "git.example:8443", "GITLAB_PROJECT": "other/project"}.get(k)):
                        self.assertEqual(Path(gitlab.require_local_project()), foreign)
                    with mock.patch.object(gitlab.config, "get", side_effect=lambda k: {"GITLAB_SITE": "git.example:443", "GITLAB_PROJECT": "other/project"}.get(k)), mock.patch.object(gitlab, "issue") as issue, mock.patch.object(gitlab, "relabel") as relabel, mock.patch.object(gitlab, "comment") as comment:
                        for command in (start.main, evidence.main):
                            with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                                command(["174"])
                        issue.assert_not_called()
                        relabel.assert_not_called()
                        comment.assert_not_called()
                    os.chdir(outside)
                    with mock.patch.object(gitlab.config, "get", side_effect=lambda k: {"GITLAB_SITE": "git.example:8443"}.get(k)):
                        self.assertEqual(Path(gitlab.require_local_project()), home)
                    with mock.patch.object(gitlab.config, "get", side_effect=lambda k: {"GITLAB_SITE": "git.example:443"}.get(k)):
                        with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                            gitlab.require_local_project()
                    for root in (home, foreign):
                        branch = subprocess.check_output(["git", "-C", str(root), "branch", "--show-current"], text=True)
                        self.assertEqual(branch.strip(), "main")
            finally:
                os.chdir(original)

    def test_start_fetch_failure_precedes_tracker_writes(self):
        from brood.dev.cli.bug import start

        with mock.patch.object(gitlab, "require_local_project", return_value="/checkout"), \
                mock.patch.object(gitlab, "issue", return_value={"labels": ["type::bug"], "title": "test"}), \
                mock.patch.object(gitlab, "relabel") as relabel, \
                mock.patch.object(gitlab, "comment") as comment, \
                mock.patch.object(start, "git", side_effect=SystemExit("fetch failed")):
            with self.assertRaisesRegex(SystemExit, "fetch failed"):
                start.main(["174"])
            relabel.assert_not_called()
            comment.assert_not_called()

    def test_nested_scopes_restore_and_override_environment(self):
        with mock.patch.object(gitlab.config, "get", return_value="env/project"):
            self.assertEqual(gitlab.project(), "env/project")
            with gitlab.project_scope("group/one"):
                self.assertEqual(gitlab.project(), "group/one")
                with gitlab.project_scope("group/two"):
                    self.assertEqual(gitlab.project(), "group/two")
                self.assertEqual(gitlab.project(), "group/one")
            self.assertEqual(gitlab.project(), "env/project")

    def test_simultaneous_scopes_are_isolated(self):
        barrier = threading.Barrier(2)

        def selected(path):
            with gitlab.project_scope(path):
                barrier.wait(timeout=5)
                return gitlab.project()

        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            a = pool.submit(selected, "group/one")
            b = pool.submit(selected, "group/two")
            self.assertEqual((a.result(), b.result()), ("group/one", "group/two"))

    def test_scope_restored_after_exception(self):
        with mock.patch.object(gitlab.config, "get", return_value="env/project"):
            with self.assertRaisesRegex(ValueError, "stop"):
                with gitlab.project_scope("group/one"):
                    raise ValueError("stop")
            self.assertEqual(gitlab.project(), "env/project")

    def test_invalid_paths_are_rejected(self):
        for path in ("", "/group/project", "group//project", "../project", "https://host/project"):
            with self.subTest(path=path), self.assertRaises(RuntimeError):
                with gitlab.project_scope(path):
                    pass

    def test_all_verbs_receive_same_scope_and_arguments(self):
        verbs = {"bug": "list show new evidence epic edit comment start close relabel labels",
                 "ci": "list show why log lint retry cancel runners set vars"}
        for group, names in verbs.items():
            for verb in names.split():
                for args in (["dev", group, "--project", "g/p", verb, "argument"],
                             ["dev", group, verb, "argument", "--project=g/p"]):
                    with self.subTest(args=args):
                        result = dispatch(args, lambda rest: (rest, gitlab.project()))
                        self.assertEqual(result, (["dev", group, verb, "argument"], "g/p"))

    def test_origin_and_environment_defaults(self):
        with mock.patch.object(gitlab, "origin", return_value="git@git.example:origin/project.git"):
            with mock.patch.object(gitlab.config, "get", return_value=None):
                self.assertEqual(gitlab.project(), "origin/project")
            with mock.patch.object(gitlab.config, "get", return_value="env/project"):
                self.assertEqual(gitlab.project(), "env/project")

    def test_bad_or_duplicate_option_fails_without_dispatch(self):
        for args in (["dev", "ci", "--project"], ["dev", "bug", "--project=", "list"],
                     ["dev", "ci", "--project=g/a", "list", "--project=g/b"]):
            callback = mock.Mock()
            with self.subTest(args=args), self.assertRaises(RuntimeError):
                dispatch(args, callback)
            callback.assert_not_called()

    def test_local_git_commands_fail_before_side_effects_for_other_project(self):
        for args in (["dev", "bug", "start", "1", "--project", "other/project"],
                     ["dev", "bug", "evidence", "1", "--project", "other/project"]):
            with self.subTest(args=args), mock.patch.object(gitlab, "origin", return_value="git@git.example:home/repo.git"), \
                    mock.patch.object(gitlab, "site", return_value="git.example"), \
                    mock.patch.object(gitlab, "issue") as issue, \
                    contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(main(args), 1)
                issue.assert_not_called()

    def test_real_checkouts_evidence_uses_validated_project(self):
        from brood.dev.cli.bug import evidence

        with tempfile.TemporaryDirectory() as temp:
            roots = [Path(temp) / name for name in ("intended", "other")]
            for root in roots:
                root.mkdir()
                subprocess.run(["git", "init", "-q", str(root)], check=True)
                subprocess.run(["git", "-C", str(root), "remote", "add", "origin",
                                "ssh://git@127.0.0.1:1/group/project.git"], check=True)
            original = os.getcwd()
            try:
                os.chdir(roots[1])
                with mock.patch.object(gitlab.config, "PROJECT", str(roots[0])), \
                        mock.patch.object(gitlab.config, "get", return_value=None), \
                        mock.patch.object(evidence, "git", wraps=evidence.git) as run:
                    with self.assertRaises(SystemExit):
                        evidence.main(["174"])
                    self.assertEqual(run.call_args.kwargs.get("cwd"), str(roots[1]))
            finally:
                os.chdir(original)

    def test_real_repositories_start_and_evidence(self):
        from brood.dev.cli.bug import evidence, start

        with tempfile.TemporaryDirectory() as temp:
            roots = [Path(temp) / name for name in ("intended", "other")]
            ssh = Path(temp) / "ssh"
            ssh.write_text('#!/bin/sh\ncd "$TEST_GIT_ROOT"\nfor arg do command=$arg; done\nexec sh -c "$command"\n')
            ssh.chmod(0o755)
            env = mock.patch.dict(os.environ, {"GIT_SSH_COMMAND": str(ssh), "TEST_GIT_ROOT": temp})
            env.start()
            self.addCleanup(env.stop)
            for index, root in enumerate(roots):
                root.mkdir()
                def git(*args):
                    return subprocess.run(["git", "-C", str(root), *args], check=True,
                                          capture_output=True, text=True).stdout.strip()
                git("init", "-q", "-b", "main")
                git("config", "user.name", "Test")
                git("config", "user.email", "test@example.com")
                git("commit", "-q", "--allow-empty", "-m", "initial")
                git("branch", f"feat/174-{'right' if index == 0 else 'wrong'}")
                remote = Path(temp) / ("group/project.git" if index == 0 else "other/project.git")
                remote.parent.mkdir(exist_ok=True)
                subprocess.run(["git", "clone", "-q", "--bare", str(root), str(remote)],
                               check=True)
                git("remote", "add", "origin", f"git@127.0.0.1:{remote.relative_to(temp)}")

            original = os.getcwd()
            try:
                os.chdir(roots[0])
                with mock.patch.object(gitlab.config, "PROJECT", str(roots[0])), \
                        mock.patch.object(gitlab.config, "get", return_value=None), \
                        contextlib.redirect_stdout(io.StringIO()) as output:
                    evidence.main(["174"])
                    self.assertIn("origin/feat/174-right", output.getvalue())
                    self.assertNotIn("origin/feat/174-wrong", output.getvalue())
                    with mock.patch.object(gitlab, "issue", return_value={
                            "labels": ["type::bug"], "title": "test"}), \
                            mock.patch.object(gitlab, "relabel"), \
                            mock.patch.object(gitlab, "comment"):
                        start.main(["174", "new"])
                    for root, expected in zip(roots, ("bug/174-new", "main")):
                        branch = subprocess.check_output(
                            ["git", "-C", str(root), "branch", "--show-current"], text=True)
                        self.assertEqual(branch.strip(), expected)

                os.chdir(roots[1])
                with mock.patch.object(gitlab.config, "PROJECT", str(roots[0])), \
                        mock.patch.object(gitlab.config, "get", return_value=None), \
                        gitlab.project_scope("other/project"), \
                        contextlib.redirect_stdout(io.StringIO()) as output, \
                        mock.patch.object(gitlab, "issue", return_value={"labels": ["type::bug"], "title": "test"}), \
                        mock.patch.object(gitlab, "relabel"), mock.patch.object(gitlab, "comment"):
                    evidence.main(["174"])
                    self.assertIn("origin/feat/174-wrong", output.getvalue())
                    self.assertNotIn("origin/feat/174-right", output.getvalue())
                    start.main(["175", "foreign"])
                    branch = subprocess.check_output(["git", "-C", str(roots[1]), "branch", "--show-current"], text=True)
                    self.assertEqual(branch.strip(), "bug/175-foreign")
                    branch = subprocess.check_output(["git", "-C", str(roots[0]), "branch", "--show-current"], text=True)
                    self.assertEqual(branch.strip(), "bug/174-new")

                os.chdir(roots[0])
                for setting, value in (("GITLAB_SITE", "other.example"),
                                       ("GITLAB_PROJECT", "other/project")):
                    with self.subTest(setting=setting), \
                            mock.patch.object(gitlab.config, "PROJECT", str(roots[0])), \
                            mock.patch.object(gitlab.config, "get", side_effect=lambda key: value if key == setting else None), \
                            mock.patch.object(gitlab, "issue") as issue, \
                            mock.patch.object(gitlab, "relabel") as relabel, \
                            mock.patch.object(gitlab, "comment") as comment:
                        with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                            start.main(["174"])
                        with self.assertRaisesRegex(RuntimeError, "origin does not match"):
                            evidence.main(["174"])
                        issue.assert_not_called()
                        relabel.assert_not_called()
                        comment.assert_not_called()
            finally:
                os.chdir(original)

    # BUG: Explicit foreign projects cannot use their current worktree.
    # HYPOTHESIS: Validation always inspects the configured brood checkout.
    def test_foreign_project_uses_current_worktree(self):
        from brood.dev.cli.bug import start

        with tempfile.TemporaryDirectory() as temp:
            home, foreign = (Path(temp) / name for name in ("home", "foreign"))
            for root, project in ((home, "home/repo"), (foreign, "other/project")):
                subprocess.run(["git", "init", "-q", str(root)], check=True)
                subprocess.run(["git", "-C", str(root), "remote", "add", "origin",
                                f"git@git.example:{project}.git"], check=True)
            original = os.getcwd()
            try:
                os.chdir(foreign)
                with mock.patch.object(gitlab.config, "PROJECT", str(home)), \
                        mock.patch.object(gitlab.config, "get", return_value=None), \
                        mock.patch.object(gitlab, "issue", return_value={"labels": ["type::bug"], "title": "test"}), \
                        mock.patch.object(gitlab, "relabel"), mock.patch.object(gitlab, "comment"), \
                        mock.patch.object(start, "git") as git, \
                        mock.patch.object(start, "default_branch", return_value="origin/main"), \
                        contextlib.redirect_stdout(io.StringIO()), gitlab.project_scope("other/project"):
                    start.main(["174"])
                    self.assertEqual(Path(git.call_args.kwargs["cwd"]).resolve(), foreign.resolve())
            finally:
                os.chdir(original)

    # SOLUTION: Select and validate the current worktree for foreign overrides.
    # RESULT: FIXED — all Git operations use the verified worktree.
    # STATUS: FIXED
    def test_foreign_linked_worktree_and_refusals(self):
        from brood.dev.cli.bug import evidence, start

        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            home, repo, linked, outside = (base / name for name in ("home", "repo", "linked", "outside"))
            for root in (home, repo):
                subprocess.run(["git", "init", "-q", "-b", "main", str(root)], check=True)
                for key, value in (("user.name", "Test"), ("user.email", "test@example.com")):
                    subprocess.run(["git", "-C", str(root), "config", key, value], check=True)
                subprocess.run(["git", "-C", str(root), "commit", "-q", "--allow-empty", "-m", "initial"], check=True)
            for root, project in ((home, "home/repo"), (repo, "other/project")):
                subprocess.run(["git", "-C", str(root), "remote", "add", "origin", f"https://git.example/{project}.git"], check=True)
            subprocess.run(["git", "-C", str(repo), "worktree", "add", "-q", "-b", "linked", str(linked)], check=True)
            outside.mkdir()
            alias = base / "alias"
            alias.symlink_to(linked, target_is_directory=True)
            original = os.getcwd()
            try:
                with mock.patch.object(gitlab.config, "PROJECT", str(home)), \
                        mock.patch.object(gitlab.config, "get", return_value=None), \
                        gitlab.project_scope("other/project"), \
                        mock.patch.object(gitlab, "issue", return_value={"labels": ["type::bug"], "title": "test"}) as issue, \
                        mock.patch.object(gitlab, "relabel") as relabel, \
                        mock.patch.object(gitlab, "comment") as comment:
                    for wrong in (home, outside):
                        os.chdir(wrong)
                        for command in (start.main, evidence.main):
                            with self.assertRaises(RuntimeError):
                                command(["174"])
                        issue.assert_not_called()
                        relabel.assert_not_called()
                        comment.assert_not_called()
                    os.chdir(alias)
                    with mock.patch.object(start, "git") as git, \
                            mock.patch.object(start, "default_branch", return_value="origin/main"), \
                            contextlib.redirect_stdout(io.StringIO()):
                        start.main(["174"])
                        self.assertEqual(Path(git.call_args.kwargs["cwd"]), linked)
                    with mock.patch.object(evidence, "git", side_effect=SystemExit) as git:
                        with self.assertRaises(SystemExit):
                            evidence.main(["174"])
                        self.assertEqual(Path(git.call_args.kwargs["cwd"]), linked)
                    os.chdir(outside)
                    with contextlib.redirect_stdout(io.StringIO()):
                        start.main(["175", "--no-branch"])
            finally:
                os.chdir(original)

    def test_both_option_placements_and_group_default(self):
        for args in (["dev", "ci", "--project", "other/project"],
                     ["dev", "ci", "--project", "other/project", "list"],
                     ["dev", "ci", "list", "--project=other/project"],
                     ["dev", "bug", "show", "1", "--project", "other/project"]):
            urls = []

            def request(method, url, path, payload=None, params=None):
                urls.append(url)
                return b'[]' if path == '/pipelines' else b'{"iid": 1, "title": "x", "state": "opened", "labels": [], "description": "", "web_url": "https://example.test/1"}'

            with self.subTest(args=args), mock.patch.object(gitlab, "request", side_effect=request), \
                    mock.patch.object(gitlab, "site", return_value="git.example"), \
                    contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(main(args), 0)
                self.assertTrue(urls)
                self.assertTrue(all('/projects/other%2Fproject/' in url for url in urls))
