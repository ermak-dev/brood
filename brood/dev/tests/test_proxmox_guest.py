"""Закреплённый браузерный клиент в allowlisted guest build context."""
# FEATURE: #160: driftty собирается из закреплённого исходника без runtime gateway.
# SPEC: Все pins доходят до buildx; неполные и неверные pins отвергаются до сборки.
# IMPLEMENTATION: Проверка формата pins и явная передача аргументов финальной сборке.
# RESULT: IMPLEMENTED — проверены allowlisted committed context и отказ до buildx.
# STATUS: DONE
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[3]


class ProxmoxGuestBuildTests(unittest.TestCase):
    def build(self, overrides):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "source"
            source.mkdir()
            for name in (".ci/proxmox-guest.sh", ".ci/go-download.sh",
                         "deploy/Dockerfile.proxmox-guest", "deploy/proxmox-guest-dhcp.sh",
                         "brood/dev/pins.json", "deploy/driftty-storage.js",
                         "deploy/driftty-storage.test.cjs"):
                target = source / name
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(ROOT / name, target)
            pins_file = source / "brood/dev/pins.json"
            pins = json.loads(pins_file.read_text())
            pins.update(overrides)
            pins_file.write_text(json.dumps(pins, indent=2) + "\n")
            main = source / "go/cmd/brood-guest/main.go"
            main.parent.mkdir(parents=True)
            main.write_text("package main\n")
            for args in (("init", "--quiet"), ("add", "."),
                         ("-c", "user.name=Test", "-c", "user.email=test@example.invalid",
                          "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "Fixture")):
                subprocess.run(["git", "-C", str(source), *args], check=True,
                               capture_output=True)
            tools = root / "tools"
            tools.mkdir()
            docker = tools / "docker"
            docker.write_text('''#!/bin/bash
set -eu
printf '%s\\n' "$@" >> "$BUILD_LOG"
for arg in "$@"; do
    case "$arg" in
        type=local,dest=*)
            dir=${arg#type=local,dest=}
            printf binary > "$dir/brood-guest"
            printf binary > "$dir/bro"
            ;;
        type=oci,dest=*) printf image > "${arg#type=oci,dest=}" ;;
    esac
done
''')
            docker.chmod(0o755)
            log = root / "build.log"
            result = subprocess.run(
                ["bash", str(source / ".ci/proxmox-guest.sh"), str(root / "out")],
                env={**os.environ, "PATH": str(tools) + os.pathsep + os.environ["PATH"],
                     "BUILD_LOG": str(log)}, capture_output=True, text=True, timeout=15)
            return result, log.read_text() if log.exists() else "", pins

    def test_driftty_pins_reach_image_build(self):
        result, log, pins = self.build({})
        self.assertEqual(result.returncode, 0, result.stderr)
        for name in ("DRIFTTY_COMMIT", "DRIFTTY_SHA256", "DRIFTTY_INDEX_SHA256",
                     "DRIFTTY_NODE_IMAGE"):
            self.assertIn(f"{name}={pins[name]}\n", log)

    def test_bad_pins_refuse_before_any_build(self):
        for key, value in (("DRIFTTY_COMMIT", "main"), ("DRIFTTY_SHA256", ""),
                           ("DRIFTTY_INDEX_SHA256", "bad"),
                           ("DRIFTTY_NODE_IMAGE", "node:latest")):
            with self.subTest(key=key):
                result, log, _ = self.build({key: value})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Invalid pinned driftty", result.stderr)
                self.assertEqual(log, "")
