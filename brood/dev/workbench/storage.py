"""Файловая ext4-петля гостя даёт cAdvisor настоящий блочный Docker root."""
import json
import os
import re
import stat
import subprocess
from pathlib import Path

from brood.dev.workbench.runtime import checked_run


def loop_node(path, kind, major, minor, stat_file=os.stat, make_node=os.mknod):
    mode, device = {"c": stat.S_IFCHR, "b": stat.S_IFBLK}[kind], os.makedev(major, minor)
    try:
        entry = stat_file(path, follow_symlinks=False)
    except FileNotFoundError:
        make_node(path, mode | 0o600, device)
        entry = stat_file(path, follow_symlinks=False)
    if stat.S_IFMT(entry.st_mode) != mode or entry.st_rdev != device:
        raise RuntimeError(f"unexpected device at {path}; refusing to replace it")


def _records(text, key):
    value = json.loads(text)
    records = value.get(key) if isinstance(value, dict) else None
    if not isinstance(records, list) or not all(isinstance(item, dict) for item in records):
        raise ValueError("invalid loop filesystem metadata")
    return records


def _backing(device, image, run):
    if not isinstance(device, str) or not re.fullmatch(r"/dev/loop[0-9]+", device):
        raise RuntimeError("Docker data root must use its private ext4 loop device")
    devices = _records(checked_run(["losetup", "--json", "--output", "NAME,BACK-FILE", device], run),
                       "loopdevices")
    if len(devices) != 1 or devices[0].get("name") != device or \
            Path(devices[0].get("back-file", "")).resolve() != image.resolve():
        raise RuntimeError("Docker loop device does not belong to this workspace")


def _existing_mount(directory, image, run):
    args = ["findmnt", "--json", "--mountpoint", str(directory), "--output", "SOURCE,FSTYPE"]
    result = run(args, capture_output=True, text=True)
    if result.returncode == 1:
        return False
    if result.returncode:
        raise RuntimeError(f"findmnt failed: {result.stderr.strip()}")
    mounts = _records(result.stdout, "filesystems")
    if len(mounts) != 1 or mounts[0].get("fstype") != "ext4":
        raise RuntimeError("Docker data root is not its private ext4 mount")
    _backing(mounts[0].get("source"), image, run)
    return True


def _image(image, run):
    if not image.exists():
        fd = os.open(image, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(fd, "wb") as output:
            output.truncate(32 * 1024 ** 3)
        checked_run(["mkfs.ext4", "-F", "-m", "0", "-L", "brood-docker", "-E",
                     "nodiscard,lazy_itable_init=1,lazy_journal_init=1", str(image)], run)
    if not image.is_file() or checked_run(["blkid", "-p", "-s", "TYPE", "-o", "value",
                                          str(image)], run) != "ext4":
        raise RuntimeError("existing Docker filesystem image is not ext4; refusing to format it")


def prepare(root, run=subprocess.run, node=loop_node):
    """Не форматировать старый файл и не прятать немонтированные данные Docker."""
    root = Path(root)
    image, directory = root / "docker.ext4", root / "docker"
    try:
        if image.is_symlink():
            raise RuntimeError("Docker filesystem image must not be a symlink")
        if directory.is_symlink():
            raise RuntimeError("Docker data root must not be a symlink")
        if _existing_mount(directory, image, run):
            return directory
        if any(directory.iterdir()):
            raise RuntimeError("refusing to hide existing Docker files behind a new filesystem")
        _image(image, run)
        node("/dev/loop-control", "c", 10, 237)
        available = checked_run(["losetup", "--find"], run)
        match = re.fullmatch(r"(/dev/loop[0-9]+)(?: \(lost\))?", available)
        if not match:
            raise RuntimeError(f"losetup did not return a guest loop device: {available!r}")
        available = match[1]
        node(available, "b", 7, int(available.removeprefix("/dev/loop")))
        device = checked_run(["losetup", "--find", "--show", "--nooverlap", str(image)], run)
        _backing(device, image, run)
        checked_run(["mount", "-t", "ext4", device, str(directory)], run)
        return directory
    except (OSError, ValueError, TypeError, KeyError) as error:
        raise RuntimeError(f"workbench Docker filesystem: {error}") from None
