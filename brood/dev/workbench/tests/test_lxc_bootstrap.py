"""LXC использует отдельный devpts и настоящий ext4, не глобальные loop-устройства."""
import importlib.util
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock


class LXCBootstrapTests(unittest.TestCase):
    def setUp(self):
        # FEATURE: Подготовка полного workbench в доверенном LXC.
        # SPEC: Приватные mount/devpts, настоящий ext4, без loop и подмены kmsg.
        self.assertIsNotNone(importlib.util.find_spec("brood.dev.workbench.bootstrap"),
                             "LXC workbench bootstrap is not implemented")
        from brood.dev.workbench import bootstrap
        self.bootstrap = bootstrap
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name) / "workspace"
        self.root.mkdir()
        (self.root / "docker").mkdir()
        self.devices = Path(self.tmp.name) / "dev"
        self.devices.mkdir()
        self.calls = []
        self.filesystem = "ext4"

    def run_command(self, args, **kwargs):
        self.calls.append(args)
        output = json.dumps({"filesystems": [{"source": "/dev/sda1[/actor/workspace]",
                                              "fstype": self.filesystem}]}) if args[0] == "findmnt" else ""
        return mock.Mock(returncode=0, stdout=output, stderr="")

    def prepare(self):
        return self.bootstrap.prepare(self.root, "lxc", run=self.run_command,
                                      device_root=self.devices)

    def test_lxc_detaches_mount_propagation_before_private_devpts_and_checks_real_kmsg(self):
        with mock.patch.object(self.bootstrap.os.path, "ismount", return_value=False), \
                mock.patch.object(self.bootstrap.runtime, "prepare_kernel_log_device") as kmsg, \
                mock.patch.object(self.bootstrap.storage, "prepare") as loop:
            self.prepare()
        self.assertIn(["mount", "--make-rprivate", "/"], self.calls)
        mount = ["mount", "-t", "devpts", "devpts", str(self.devices / "pts"),
                 "-o", "newinstance,ptmxmode=0666,mode=0620,nosuid,noexec"]
        self.assertIn(mount, self.calls)
        self.assertLess(self.calls.index(["mount", "--make-rprivate", "/"]), self.calls.index(mount))
        self.assertEqual((self.devices / "ptmx").readlink(), Path("pts/ptmx"))
        self.assertEqual((self.devices / "pts").stat().st_mode & 0o777, 0o755)
        kmsg.assert_called_once_with()
        loop.assert_not_called()
        self.assertFalse(any(args[0] in ("losetup", "mkfs.ext4", "mknod") for args in self.calls))

    def test_lxc_refuses_non_ext4_or_unknown_filesystem_before_mount_changes(self):
        for filesystem in ("overlay", "virtiofs", "tmpfs", ""):
            self.filesystem = filesystem
            self.calls.clear()
            with self.subTest(filesystem=filesystem), self.assertRaisesRegex(RuntimeError, "ext4"):
                self.prepare()
            self.assertFalse(any(args[0] == "mount" for args in self.calls))

    def test_lxc_does_not_hide_or_reuse_legacy_loop_storage(self):
        (self.root / "docker.ext4").write_text("old filesystem")
        with self.assertRaisesRegex(RuntimeError, "microVM|loop"):
            self.prepare()
        self.assertEqual((self.root / "docker.ext4").read_text(), "old filesystem")
        self.assertFalse(self.calls)

    def test_lxc_refuses_symlink_data_root_or_terminal_directory(self):
        (self.root / "docker").rmdir()
        (self.root / "docker").symlink_to(self.devices, target_is_directory=True)
        with self.assertRaisesRegex(RuntimeError, "symlink"):
            self.prepare()
        (self.root / "docker").unlink()
        (self.root / "docker").mkdir()
        (self.devices / "pts").symlink_to(self.root, target_is_directory=True)
        with self.assertRaisesRegex(RuntimeError, "symlink"):
            self.prepare()

    def test_lxc_refuses_unknown_ptmx_without_replacing_it(self):
        (self.devices / "ptmx").write_text("do not replace")
        with self.assertRaisesRegex(RuntimeError, "ptmx"):
            self.prepare()
        self.assertEqual((self.devices / "ptmx").read_text(), "do not replace")

    def test_existing_private_terminal_is_not_remounted(self):
        (self.devices / "pts").mkdir()
        (self.devices / "ptmx").symlink_to("pts/ptmx")
        with mock.patch.object(self.bootstrap.os.path, "ismount", return_value=True), \
                mock.patch.object(self.bootstrap.runtime, "prepare_kernel_log_device"):
            self.prepare()
        self.assertFalse(any(args[:3] == ["mount", "-t", "devpts"] for args in self.calls))

    def test_unknown_sandbox_never_falls_back_to_global_loop_storage(self):
        with mock.patch.object(self.bootstrap.runtime, "prepare_kernel_log_device") as kmsg, \
                mock.patch.object(self.bootstrap.storage, "prepare") as loop:
            for sandbox in ("gvisor", "docker", "unknown"):
                with self.subTest(sandbox=sandbox), self.assertRaisesRegex(RuntimeError, "runtime"):
                    self.bootstrap.prepare(self.root, sandbox)
        kmsg.assert_not_called()
        loop.assert_not_called()

    def test_device_filesystem_errors_have_a_reason_without_traceback(self):
        with mock.patch.object(Path, "mkdir", side_effect=PermissionError("denied")):
            with self.assertRaisesRegex(RuntimeError, "LXC.*denied"):
                self.prepare()

    def test_microvm_bootstrap_retains_its_existing_private_kernel_storage_contract(self):
        with mock.patch.object(self.bootstrap.runtime, "prepare_kernel_log_device") as kmsg, \
                mock.patch.object(self.bootstrap.storage, "prepare") as loop:
            self.bootstrap.prepare(self.root, "", run=self.run_command, device_root=self.devices)
        kmsg.assert_called_once_with()
        loop.assert_called_once_with(self.root)
        self.assertFalse(self.calls)
