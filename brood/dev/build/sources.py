"""Снимки Git без локальных настроек, секретов и изменений рабочего дерева."""

import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

from brood.dev.build.plan import repository


def git(root, *arguments, missing=False):
    """Не отдаёт stderr Git: тот может содержать credentials из конфигурации."""
    result = subprocess.run(["git", "-C", str(root), *arguments],
                            capture_output=True)
    if result.returncode:
        if missing:
            return None
        raise RuntimeError(f"Git {arguments[0]} failed in the selected source")
    try:
        return result.stdout.decode("utf-8")
    except UnicodeError:
        raise RuntimeError("Git source contains invalid UTF-8 paths or text") from None


def revision(root, ref):
    value = git(root, "rev-parse", "--verify", "--end-of-options", f"{ref}^{{commit}}").strip()
    if not re.fullmatch(r"[0-9a-f]{40}", value):
        raise RuntimeError("Source must resolve to a full SHA-1 Git commit")
    return value


def origin(root, required=False):
    value = git(root, "remote", "get-url", "origin", missing=True)
    if value is None:
        if required:
            raise RuntimeError("Git metadata requires a safe origin URL")
        return None
    return repository(value.strip())


def _check_tree(root, sha):
    paths = git(root, "ls-tree", "-rz", "--name-only", sha).split("\0")
    for path in filter(None, paths):
        parts = Path(path).parts
        if parts[0] in {".env", "secrets", ".ate-dev-env.sh", ".brood"}:
            raise RuntimeError("Source commit contains secret settings or generated build output")
        if any(ord(char) < 32 or ord(char) == 127 for char in path):
            raise RuntimeError("Source commit contains control characters in paths")


def _check_links(root):
    for directory, dirs, files in os.walk(root, followlinks=False):
        dirs[:] = [name for name in dirs if not (Path(directory) == root and name == ".git")]
        for name in [*dirs, *files]:
            path = Path(directory) / name
            if path.is_symlink():
                try:
                    path.resolve().relative_to(root)
                except (ValueError, RuntimeError):
                    raise RuntimeError("Source symlink escapes its snapshot") from None


def snapshot(source, ref, target, git_metadata=False):
    """Создаёт новый снимок точного commit, не копируя Git-конфигурацию клона."""
    source, target = Path(source).resolve(), Path(target)
    if os.path.lexists(target):
        raise RuntimeError("Source snapshot destination already exists")
    sha = revision(source, ref)
    remote = origin(source, required=git_metadata)
    _check_tree(source, sha)
    target.parent.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=".snapshot-", dir=target.parent))
    try:
        git(staging, "init", "-q", "--template=")
        git(staging, "-c", "core.hooksPath=/dev/null", "fetch", "-q", "--depth=1",
            "--update-shallow", "--no-tags", "--", str(source), sha)
        git(staging, "-c", "core.hooksPath=/dev/null", "-c", "core.autocrlf=false",
            "-c", "core.logAllRefUpdates=false", "checkout", "-q", "--detach", sha)
        _check_links(staging)
        if git_metadata:
            # Stat-поля index не должны превращать один commit в разные OCI-входы.
            (staging / ".git/index").unlink()
            git(staging, "read-tree", "--reset", sha)
            git(staging, "remote", "add", "origin", remote)
            (staging / ".git/FETCH_HEAD").unlink(missing_ok=True)
        else:
            shutil.rmtree(staging / ".git")
        staging.rename(target)
    finally:
        if staging.exists():
            shutil.rmtree(staging)
    return {"revision": sha, "gitMetadata": git_metadata, **({"repository": remote} if remote else {})}
