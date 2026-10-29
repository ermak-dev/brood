"""Восстановление pinned Go bootstrap после частичного сетевого отказа."""
# BUG: #83: одиночный curl теряет частичную загрузку при сетевом EOF.
# HYPOTHESIS: повтор с продолжением сохранённого файла переживёт временный отказ.
# SOLUTION: общий загрузчик возобновляет собственный temporary archive и сверяет SHA-256.
# RESULT: FIXED — настоящий curl продолжает HTTP-ответ после обрыва, плохой архив не извлекается.
# STATUS: FIXED
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
import shutil
import socket
import tarfile
import threading
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[3]
CURL = r'''#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$CURL_CALLS"
out= resume=no
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output|-o) out=$2; shift ;;
        --continue-at|-C) [ "$2" = - ]; resume=yes; shift ;;
    esac
    shift
done
[ -n "$out" ]
case "$CURL_MODE" in
    resume)
        if [ ! -e "$CURL_STATE" ]; then
            printf first > "$out"
            : > "$CURL_STATE"
            printf 'simulated unexpected EOF\n' >&2
            exit 56
        fi
        [ "$resume" = yes ] || exit 91
        [ "$(cat "$out")" = first ] || exit 92
        printf second >> "$out"
        ;;
    corrupt) printf corrupted > "$out" ;;
    failure)
        printf first > "$out"
        printf 'simulated connection reset\n' >&2
        exit 56
        ;;
esac
'''


class GoDownloadTests(unittest.TestCase):
    def bootstrap(self, mode, *, script=CURL, payload=b"firstsecond", values=None, real_tar=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            tools = root / "tools"
            tools.mkdir()
            commands = {"curl": script, "sleep": "#!/bin/sh\nexit 0\n"}
            if not real_tar:
                commands["tar"] = '#!/bin/sh\nprintf extracted > "$TAR_CALLED"\n'
            for name, body in commands.items():
                executable = tools / name
                executable.write_text(body)
                executable.chmod(0o755)
            pins = root / "brood/dev/pins.json"
            pins.parent.mkdir(parents=True)
            pins.write_text(json.dumps({"GO_VERSION": "1.27.3", "GO_SHA256":
                                       hashlib.sha256(payload).hexdigest()}))
            work = root / "work"
            work.mkdir()
            env = {**os.environ, "PATH": str(tools) + os.pathsep + os.environ["PATH"],
                   "CURL_MODE": mode, "CURL_STATE": str(root / "state"),
                   "CURL_CALLS": str(root / "calls"), "TAR_CALLED": str(root / "extracted")}
            env.update(values or {})
            result = subprocess.run(
                ["bash", "-ceu", 'work="$1"; source "$2"', "bootstrap", str(work),
                 str(ROOT / ".ci/go-toolchain.sh")], cwd=root, env=env,
                capture_output=True, text=True, timeout=10)
            calls = (root / "calls").read_text().splitlines()
            extracted = (work / "toolchain/go/VERSION").is_file() if real_tar else (
                root / "extracted").exists()
            return result, calls, extracted

    def test_partial_eof_resumes_the_same_archive_before_extraction(self):
        result, calls, extracted = self.bootstrap("resume")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 2)
        self.assertTrue(extracted)

    def test_repeated_failure_is_bounded_and_never_extracts(self):
        result, calls, extracted = self.bootstrap("failure")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(calls), 8)
        self.assertFalse(extracted)

    def test_real_curl_resumes_an_interrupted_http_response_and_extracts_checked_tar(self):
        content = hashlib.shake_256(b"Go archive fixture").digest(65536)
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w:gz") as stream:
            entry = tarfile.TarInfo("go/VERSION")
            entry.size = len(content)
            stream.addfile(entry, io.BytesIO(content))
        payload = archive.getvalue()
        ranges = []

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_GET(self):
                request_range = self.headers.get("Range")
                ranges.append(request_range)
                offset = int(request_range.removeprefix("bytes=").removesuffix("-")) if (
                    request_range) else 0
                self.send_response(206 if offset else 200)
                self.send_header("Content-Length", str(len(payload) - offset))
                if offset:
                    self.send_header("Content-Range", f"bytes {offset}-{len(payload) - 1}/{len(payload)}")
                self.end_headers()
                if len(ranges) == 1:
                    self.wfile.write(payload[:32768])
                    self.wfile.flush()
                    self.connection.shutdown(socket.SHUT_RDWR)
                    self.connection.close()
                else:
                    self.wfile.write(payload[offset:])

        script = '''#!/bin/bash
set -eu
printf '%s\\n' "$*" >> "$CURL_CALLS"
args=()
for arg in "$@"; do
    case "$arg" in
        https://go.dev/dl/*) args+=("$FIXTURE_URL") ;;
        *) args+=("$arg") ;;
    esac
done
exec "$REAL_CURL" "${args[@]}"
'''
        with ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                result, calls, extracted = self.bootstrap(
                    "http", script=script, payload=payload, real_tar=True,
                    values={"REAL_CURL": shutil.which("curl"),
                            "FIXTURE_URL": f"http://127.0.0.1:{server.server_port}/go.tar.gz"})
            finally:
                server.shutdown()
                thread.join()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(ranges, [None, "bytes=32768-"])
        self.assertEqual(len(calls), 2)
        self.assertTrue(extracted)

    def test_image_recipes_share_the_checked_resumable_downloader(self):
        for name, source in (("deploy/Dockerfile.go", ".ci/go-download.sh"),
                             ("brood/dev/workbench/Dockerfile", "brood/.ci/go-download.sh")):
            with self.subTest(recipe=name):
                recipe = (ROOT / name).read_text()
                self.assertIn(f"COPY {source} /tmp/go-download.sh", recipe)
                self.assertIn("bash /tmp/go-download.sh", recipe)
                self.assertNotIn('curl -fsSL \"https://go.dev/dl/', recipe)

    def test_checksum_mismatch_never_extracts_or_retries_bad_content(self):
        result, calls, extracted = self.bootstrap("corrupt")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(calls), 1)
        self.assertFalse(extracted)
