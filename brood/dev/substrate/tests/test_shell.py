"""Процессы установки: зависший бинарный поток не держит выкладку вечно (#246).

Процесс настоящий: он не читает stdin, и запись потока в его pipe встаёт,
как встал kubectl exec к умирающему поду.
"""
import os
import sys
import tempfile
import time
import unittest

from brood.dev.substrate import local

# Не читает stdin и спит: поток больше буфера pipe не уйдёт никогда.
STALLED = "import os, sys, time; open(sys.argv[1], 'w').write(str(os.getpid())); time.sleep(60)"


class OutputBytesTimeoutTests(unittest.TestCase):
    def test_stalled_stream_is_killed_and_refused_after_timeout(self):
        with tempfile.TemporaryDirectory() as directory:
            pidfile = os.path.join(directory, "pid")
            shell = local.Shell(dict(os.environ), directory)
            started = time.monotonic()
            with self.assertRaises(TimeoutError) as raised:
                shell.output_bytes((sys.executable, "-c", STALLED, pidfile),
                                   input=b"x" * (4 << 20), timeout=2)
            self.assertLess(time.monotonic() - started, 20)
            self.assertIn("timed out after 2 s", str(raised.exception))
            with open(pidfile) as f:
                pid = int(f.read())
        # Процесс убит и собран: зависший kubectl не переживает отказ выкладки.
        with self.assertRaises(ProcessLookupError):
            os.kill(pid, 0)

    def test_finished_stream_returns_code_and_output(self):
        shell = local.Shell(dict(os.environ), None)
        code, out, err = shell.output_bytes(
            (sys.executable, "-c", "import sys; data = sys.stdin.buffer.read(); print(len(data)); sys.exit(3)"),
            input=b"\0" * 1000, timeout=30)
        self.assertEqual((code, out.strip(), err), (3, "1000", ""))


if __name__ == "__main__":
    unittest.main()
