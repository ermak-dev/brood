"""Гостевой kernel log нужен kubelet; фиктивное устройство недопустимо."""
import stat
import unittest
from unittest import mock

from brood.dev.workbench import runtime


class KernelLogTests(unittest.TestCase):
    def setUp(self):
        self.valid = mock.Mock(st_mode=stat.S_IFCHR | 0o600, st_rdev=runtime.os.makedev(1, 11))
        self.stat = mock.Mock(return_value=self.valid)
        self.make = mock.Mock()
        self.open = mock.Mock(return_value=42)
        self.close = mock.Mock()

    def prepare(self):
        return runtime.prepare_kernel_log_device(stat_file=self.stat, make_node=self.make,
                                                open_device=self.open, close_device=self.close)

    def test_missing_node_is_created_and_actual_device_access_is_checked(self):
        self.stat.side_effect = [FileNotFoundError(), self.valid]
        self.prepare()
        self.make.assert_called_once_with("/dev/kmsg", stat.S_IFCHR | 0o600,
                                          runtime.os.makedev(1, 11))
        self.open.assert_called_once_with("/dev/kmsg", runtime.os.O_RDONLY | runtime.os.O_NONBLOCK)
        self.close.assert_called_once_with(42)

    def test_existing_kernel_device_is_not_replaced(self):
        self.prepare()
        self.make.assert_not_called()
        self.stat.assert_called_once_with("/dev/kmsg", follow_symlinks=False)
        self.close.assert_called_once_with(42)

    def test_regular_files_symlinks_and_other_devices_are_rejected(self):
        for mode, device in [(stat.S_IFREG, 0), (stat.S_IFLNK, 0),
                             (stat.S_IFCHR, runtime.os.makedev(1, 3))]:
            with self.subTest(mode=mode, device=device):
                self.stat.return_value = mock.Mock(st_mode=mode, st_rdev=device)
                with self.assertRaisesRegex(RuntimeError, "c 1:11"):
                    self.prepare()
                self.make.assert_not_called()
                self.open.assert_not_called()

    def test_access_denial_is_reported_without_a_substitute_device(self):
        self.open.side_effect = PermissionError(1, "Operation not permitted")
        with self.assertRaisesRegex(RuntimeError, "guest.*kmsg.*Operation not permitted"):
            self.prepare()
        self.make.assert_not_called()
        self.close.assert_not_called()
