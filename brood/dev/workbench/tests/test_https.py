"""Приватная HTTPS-проверка обязательна до успешного результата гостя."""
# BUG: #87: rollout без проверки CA/hostname разрешает ложный passed.
# HYPOTHESIS: отдельный bounded probe до passed проверит настоящий TLS endpoint.
# SOLUTION: CA/hostname probe ждёт свой случайный port-forward и убирает только его PID.
# RESULT: FIXED — unit и реальные TLS-проверки отвергают чужой CA и неверное имя.
# STATUS: FIXED
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[4]
KUBECTL = '''#!/bin/sh
set -eu
printf '%s\\n' "$*" > "$FORWARD_ARGS"
printf '%s\\n' "$$" > "$FORWARD_PID"
case "$FORWARD_MODE" in
    exit) exit 1 ;;
    no_ready) exec /usr/bin/sleep 60 ;;
    delayed) /usr/bin/sleep 0.1 ;;
esac
printf 'Forwarding from 127.0.0.1:43123 -> 8443\\n'
exec /usr/bin/sleep 60
'''
CURL = '''#!/bin/sh
set -eu
for i in $(seq 1 100); do
    [ -f "$FORWARD_PID" ] && break
    /usr/bin/sleep 0.01
done
printf '%s\\n' "$@" > "$CURL_ARGS"
case "$PROBE_MODE" in
    success) body=ok; code=200 ;;
    wrong_body) body=other; code=200 ;;
    wrong_status) body=ok; code=201 ;;
    *) exit 60 ;;
esac
while [ "$#" -gt 0 ]; do
    if [ "$1" = --output ]; then printf '%s\\n' "$body" > "$2"; break; fi
    shift
done
printf '%s' "$code"
'''


class WorkbenchHTTPSTests(unittest.TestCase):
    def run_probe(self, mode, guest="1", forward_mode="ready"):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ("logs", "brood/secrets", "substrate/bin/substrate", "tools"):
                (root / name).mkdir(parents=True, exist_ok=True)
            (root / "brood/secrets/tls.crt").write_text("fixture CA")
            (root / "substrate/bin/substrate/kubeconfig").write_text("fixture config")
            scripts = {"substrate/bin/substrate/kubectl": KUBECTL, "tools/curl": CURL,
                       "tools/sleep": "#!/bin/sh\nexec /usr/bin/sleep 0.01\n"}
            for name, body in scripts.items():
                path = root / name
                path.write_text(body)
                path.chmod(0o700)
            env = {**os.environ, "WORKBENCH_ROOT": str(root), "BROOD_WORKBENCH": guest,
                   "SUBSTRATE_SOURCE": str(root / "substrate"),
                   "DOCKER_HOST": "unix:///var/run/docker.sock",
                   "PATH": str(root / "tools") + os.pathsep + os.environ["PATH"],
                   "FORWARD_PID": str(root / "forward.pid"),
                   "FORWARD_ARGS": str(root / "forward.args"),
                   "CURL_ARGS": str(root / "curl.args"), "PROBE_MODE": mode,
                   "FORWARD_MODE": forward_mode}
            result = subprocess.run(["bash", str(ROOT / ".ci/workbench-https.sh")], env=env,
                                    capture_output=True, text=True, timeout=10)
            pid_file = root / "forward.pid"
            if pid_file.exists():
                with self.assertRaises(ProcessLookupError, msg="own forwarder must be stopped"):
                    os.kill(int(pid_file.read_text()), 0)
            paths = (root / "forward.args", root / "curl.args")
            return result, [p.read_text().splitlines() if p.exists() else [] for p in paths]

    def test_guest_checks_https_before_writing_passed(self):
        script = (ROOT / ".ci/workbench.sh").read_text()
        probe = "bash .ci/workbench-https.sh"
        self.assertIn(probe, script)
        self.assertLess(script.index("bin/brood deploy go"), script.index(probe))
        self.assertLess(script.index(probe), script.index("status passed"))

    def test_success_uses_own_ca_and_loopback_forwarder(self):
        result, (forward, curl) = self.run_probe("success")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("HTTPS check passed", result.stdout)
        self.assertIn("--address=127.0.0.1", forward[0])
        self.assertIn("service/brood-go :8443", forward[0])
        self.assertIn("--cacert", curl)
        self.assertTrue(curl[curl.index("--cacert") + 1].endswith("/brood/secrets/tls.crt"))
        self.assertIn("https://127.0.0.1:43123/healthz", curl)
        self.assertNotIn("--insecure", curl)
        self.assertNotIn("-k", curl)

    def test_tls_failure_fails_probe_and_removes_forwarder(self):
        result, (_, curl) = self.run_probe("failure")
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(curl, "TLS verification must actually be attempted")
        self.assertNotIn("HTTPS check passed", result.stdout)

    def test_wrong_body_or_status_never_passes(self):
        for mode in ("wrong_body", "wrong_status"):
            with self.subTest(mode=mode):
                result, (_, curl) = self.run_probe(mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertTrue(curl)
                self.assertNotIn("HTTPS check passed", result.stdout)

    def test_no_ready_or_failed_forwarder_never_probes_an_existing_listener(self):
        for mode in ("no_ready", "exit"):
            with self.subTest(mode=mode):
                result, (_, curl) = self.run_probe("success", forward_mode=mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(curl, "Do not probe a port not opened by this forwarder")

    def test_delayed_forwarder_uses_only_its_allocated_port(self):
        result, (_, curl) = self.run_probe("success", forward_mode="delayed")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("https://127.0.0.1:43123/healthz", curl)

    def test_probe_refuses_non_guest_before_starting_forwarder(self):
        result, (forward, curl) = self.run_probe("success", guest="0")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(forward)
        self.assertFalse(curl)
