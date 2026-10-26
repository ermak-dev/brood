"""Docker root должен иметь блочную ФС, а не virtio-fs с device=none."""
import json
import stat
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from brood.dev.workbench import storage


class DockerStorageTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.data = self.root / "docker"
        self.data.mkdir()
        self.image = self.root / "docker.ext4"
        self.calls = []
        self.node = mock.Mock()
        self.mounted = None
        self.mount_error = False
        self.filesystem = "ext4"
        self.available = "/dev/loop0"

    def run_command(self, args, **kwargs):
        self.calls.append(args)
        output, code, error = "", 0, ""
        if args[0] == "findmnt":
            code = 0 if self.mounted else 1
            output = json.dumps({"filesystems": [self.mounted]}) if self.mounted else ""
        elif args[0] == "blkid":
            output = self.filesystem
        elif args[:2] == ["losetup", "--json"]:
            output = json.dumps({"loopdevices": [{"name": "/dev/loop0", "back-file": str(self.image)}]})
        elif args[0] == "losetup" and "--find" in args:
            output = "/dev/loop0" if "--show" in args else self.available
        elif args[0] == "mount" and self.mount_error:
            code, error = 1, "unsupported filesystem"
        return mock.Mock(returncode=code, stdout=output, stderr=error)

    def prepare(self):
        return storage.prepare(self.root, run=self.run_command, node=self.node)

    def test_new_sparse_image_is_private_formatted_and_mounted(self):
        self.assertEqual(self.prepare(), self.data)
        self.assertEqual(self.image.stat().st_size, 32 * 1024 ** 3)
        self.assertEqual(self.image.stat().st_mode & 0o777, 0o600)
        self.assertEqual(sum(args[0] == "mkfs.ext4" for args in self.calls), 1)
        self.node.assert_any_call("/dev/loop-control", "c", 10, 237)
        self.node.assert_any_call("/dev/loop0", "b", 7, 0)
        self.assertIn(["mount", "-t", "ext4", "/dev/loop0", str(self.data)], self.calls)

    def test_kernel_loop_without_a_device_node_is_created_before_attachment(self):
        self.available = "/dev/loop0 (lost)"
        self.assertEqual(self.prepare(), self.data)
        self.node.assert_any_call("/dev/loop0", "b", 7, 0)
        self.assertIn(["mount", "-t", "ext4", "/dev/loop0", str(self.data)], self.calls)

    def test_existing_image_is_never_reformatted(self):
        self.image.write_bytes(b"existing workspace data")
        self.prepare()
        self.assertEqual(self.image.read_bytes(), b"existing workspace data")
        self.assertFalse(any(args[0] == "mkfs.ext4" for args in self.calls))

    def test_already_mounted_own_image_is_reused(self):
        self.image.write_bytes(b"existing")
        self.mounted = {"source": "/dev/loop0", "fstype": "ext4"}
        self.assertEqual(self.prepare(), self.data)
        self.node.assert_not_called()
        self.assertFalse(any(args[0] in ("mount", "mkfs.ext4") for args in self.calls))

    def test_other_mount_is_not_replaced(self):
        self.mounted = {"source": "none", "fstype": "virtiofs"}
        with self.assertRaisesRegex(RuntimeError, "private ext4"):
            self.prepare()
        self.node.assert_not_called()
        self.assertFalse(self.image.exists())

    def test_existing_unmounted_docker_files_are_not_hidden(self):
        (self.data / "existing").write_text("work")
        with self.assertRaisesRegex(RuntimeError, "existing Docker"):
            self.prepare()
        self.assertFalse(self.image.exists())

    def test_invalid_existing_image_is_not_formatted(self):
        self.image.write_bytes(b"not ext4")
        self.filesystem = "xfs"
        with self.assertRaisesRegex(RuntimeError, "not ext4"):
            self.prepare()
        self.assertEqual(self.image.read_bytes(), b"not ext4")
        self.assertFalse(any(args[0] == "mkfs.ext4" for args in self.calls))

    def test_failed_mount_keeps_a_possibly_reused_association_for_inspection(self):
        self.mount_error = True
        with self.assertRaisesRegex(RuntimeError, "unsupported filesystem"):
            self.prepare()
        self.assertFalse(any(args[:2] == ["losetup", "--detach"] for args in self.calls))
        self.assertTrue(self.image.exists())

    def test_mounted_image_symlink_is_not_accepted_as_owned_storage(self):
        target = self.root / "other-image"
        target.write_text("other")
        self.image.symlink_to(target)
        self.mounted = {"source": "/dev/loop0", "fstype": "ext4"}
        with self.assertRaisesRegex(RuntimeError, "symlink"):
            self.prepare()
        self.node.assert_not_called()

    def test_malformed_mount_metadata_has_a_reason_without_a_traceback(self):
        run = mock.Mock(return_value=mock.Mock(returncode=0, stdout="[]", stderr=""))
        with self.assertRaisesRegex(RuntimeError, "filesystem"):
            storage.prepare(self.root, run=run, node=self.node)
        self.node.assert_not_called()

    def test_missing_loop_device_is_created_and_other_nodes_are_refused(self):
        device = mock.Mock(st_mode=stat.S_IFBLK | 0o600, st_rdev=storage.os.makedev(7, 0))
        make = mock.Mock()
        probe = mock.Mock(side_effect=[FileNotFoundError(), device])
        storage.loop_node("/dev/loop0", "b", 7, 0, stat_file=probe, make_node=make)
        make.assert_called_once_with("/dev/loop0", stat.S_IFBLK | 0o600, storage.os.makedev(7, 0))
        with self.assertRaisesRegex(RuntimeError, "unexpected device"):
            storage.loop_node("/dev/loop0", "b", 7, 0,
                              stat_file=mock.Mock(return_value=mock.Mock(st_mode=stat.S_IFLNK, st_rdev=0)),
                              make_node=make)
        self.assertEqual(make.call_count, 1)
