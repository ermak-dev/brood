"""Подготовка устройств и хранилища для выбранного ядра workbench."""
import json
import os
import subprocess
from pathlib import Path

from brood.dev.workbench import runtime, storage


def _lxc_paths(root, devices):
    directory = root / "docker"
    image = root / "docker.ext4"
    if directory.is_symlink():
        raise RuntimeError("LXC Docker data root must not be a symlink")
    if image.exists() or image.is_symlink():
        raise RuntimeError("LXC workbench requires a fresh workspace without microVM loop storage")
    pts, ptmx = devices / "pts", devices / "ptmx"
    if pts.is_symlink():
        raise RuntimeError("LXC terminal directory must not be a symlink")
    if ptmx.is_symlink():
        if ptmx.readlink() != Path("pts/ptmx"):
            raise RuntimeError("LXC ptmx must point to its private pts/ptmx")
    elif ptmx.exists():
        raise RuntimeError("LXC ptmx must point to its private pts/ptmx")
    return directory, pts, ptmx


def _require_ext4(directory, run):
    output = runtime.checked_run(["findmnt", "--json", "--target", str(directory),
                                  "--output", "SOURCE,FSTYPE"], run)
    try:
        mounts = json.loads(output)["filesystems"]
        if len(mounts) != 1 or mounts[0]["fstype"] != "ext4" or \
                not mounts[0]["source"].startswith("/dev/"):
            raise ValueError
    except (ValueError, TypeError, KeyError, AttributeError):
        raise RuntimeError("LXC Docker data root requires a real ext4 durable workspace") from None


def prepare(root, sandbox, run=subprocess.run, device_root=Path("/dev")):
    """LXC не получает loop-устройства общего ядра; microVM хранит старый контракт."""
    root = Path(root)
    if sandbox not in ("", "microvm", "lxc"):
        raise RuntimeError(f"unsupported workbench container runtime: {sandbox}")
    if sandbox != "lxc":
        runtime.prepare_kernel_log_device()
        storage.prepare(root)
        return
    try:
        directory, pts, ptmx = _lxc_paths(root, Path(device_root))
        _require_ext4(directory, run)
        # Исходные mount peer groups LXC могут быть общими с воркером.
        runtime.checked_run(["mount", "--make-rprivate", "/"], run)
        pts.mkdir(mode=0o755, exist_ok=True)
        pts.chmod(0o755)
        if not os.path.ismount(pts):
            runtime.checked_run(["mount", "-t", "devpts", "devpts", str(pts), "-o",
                                 "newinstance,ptmxmode=0666,mode=0620,nosuid,noexec"], run)
        if not ptmx.is_symlink():
            ptmx.symlink_to("pts/ptmx")
        runtime.prepare_kernel_log_device()
    except OSError as error:
        raise RuntimeError(f"LXC workbench preparation failed: {error}") from None
