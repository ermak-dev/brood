"""Сетевой retry в CI не повторяет и не пропускает проверки Go."""
# BUG: #85: transient module transport failure обрушает весь холодный CI.
# HYPOTHESIS: bounded retry только mod download сохранит проверенный cache.
# SOLUTION: три ограниченных попытки до проверок, с сохранением одного cache.
# RESULT: FIXED — транспортный отказ повторяется, make test failure не ретраится.
# STATUS: FIXED
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[3]
GO = r'''#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GO_CALLS"
[ "$*" = 'mod download' ]
if [ ! -f "$GOMODCACHE/verified" ]; then
    printf verified > "$GOMODCACHE/verified"
    printf 'simulated module transport failure\n' >&2
    exit 1
fi
[ "$(cat "$GOMODCACHE/verified")" = verified ]
[ "$GO_MODE" = recover ] || exit 1
'''
TIMEOUT = r'''#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$TIMEOUT_CALLS"
while [ "$#" -gt 0 ]; do
    case "$1" in
        --*) shift ;;
        *s) shift; break ;;
        *) break ;;
    esac
done
exec "$@"
'''


class CIModuleDownloadTests(unittest.TestCase):
    def run_ci(self, mode, fail_check=""):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / ".ci").mkdir()
            (root / "go").mkdir()
            shutil.copyfile(ROOT / ".ci/go-test.sh", root / ".ci/go-test.sh")
            (root / ".ci/go-toolchain.sh").write_text(
                'export GOMODCACHE="$work/modules"\nmkdir "$GOMODCACHE"\n')
            tools = root / "tools"
            tools.mkdir()
            scripts = {"go": GO, "timeout": TIMEOUT, "sleep": "#!/bin/sh\nexit 0\n",
                       "tar": "#!/bin/sh\nexit 0\n", "make": '''#!/bin/sh
set -eu
printf '%s\\n' "$*" >> "$MAKE_CALLS"
[ "$*" != "$FAIL_CHECK" ] || exit 7
'''}
            for name, body in scripts.items():
                executable = tools / name
                executable.write_text(body)
                executable.chmod(0o755)
            env = {**os.environ, "PATH": str(tools) + os.pathsep + os.environ["PATH"],
                   "GO_MODE": mode, "FAIL_CHECK": fail_check, "GO_CALLS": str(root / "go-calls"),
                   "MAKE_CALLS": str(root / "make-calls"), "TIMEOUT_CALLS": str(root / "timeouts")}
            result = subprocess.run(["bash", ".ci/go-test.sh", "--inside"], cwd=root, env=env,
                                    capture_output=True, text=True, timeout=10)

            def lines(name):
                path = root / name
                return path.read_text().splitlines() if path.exists() else []

            return result, lines("go-calls"), lines("make-calls"), lines("timeouts")

    def test_module_transport_failure_retries_in_same_cache_before_checks(self):
        result, calls, checks, timeouts = self.run_ci("recover")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls, ["mod download", "mod download"])
        self.assertEqual(checks, ["fmt-check", "vet", "test", "build", "cross"])
        self.assertEqual(len(timeouts), 2)
        self.assertTrue(all("600s go mod download" in call for call in timeouts))

    def test_exhausted_module_attempts_never_run_checks(self):
        result, calls, checks, timeouts = self.run_ci("failure")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(calls), 3)
        self.assertFalse(checks)
        self.assertEqual(len(timeouts), 3)

    def test_test_failure_is_not_retried_or_ignored(self):
        result, calls, checks, _ = self.run_ci("recover", "test")
        self.assertEqual(result.returncode, 7)
        self.assertEqual(len(calls), 2)
        self.assertEqual(checks, ["fmt-check", "vet", "test"])

    def test_job_budget_contains_bounded_bootstrap_and_all_checks(self):
        configuration = yaml.safe_load((ROOT / ".gitlab-ci.yml").read_text())
        self.assertEqual(configuration["go-test"]["timeout"], "90 minutes")
