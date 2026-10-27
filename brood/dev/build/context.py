"""Новый переносимый Docker-контекст из явно выбранных Git-источников."""

import copy
import json
import os
from pathlib import Path
import shutil
import tempfile

from brood.dev.build import sources
from brood.dev.build.plan import repository


def _external(spec, target, cache):
    url = spec["repository"]
    if url in cache:
        cached = Path(cache[url])
        if not cached.is_absolute():
            raise RuntimeError("BUILD_SOURCE_CACHE paths must be absolute")
        if sources.origin(cached, required=True) != url:
            raise RuntimeError("BUILD_SOURCE_CACHE checkout origin does not match its repository")
        return sources.snapshot(cached, spec["revision"], target, spec["gitMetadata"])
    with tempfile.TemporaryDirectory(prefix="brood-git-") as directory:
        sources.git(directory, "init", "-q", "--template=")
        sources.git(directory, "remote", "add", "origin", repository(url))
        sources.git(directory, "-c", "core.hooksPath=/dev/null", "fetch", "-q", "--no-tags",
                    "--depth=1", "origin", spec["revision"])
        return sources.snapshot(directory, spec["revision"], target, spec["gitMetadata"])


def export(project, destination, cache=None):
    """Публикует контекст целиком; при ошибке целевой путь не появляется."""
    cache = {} if cache is None else cache
    if not isinstance(cache, dict) or any(not isinstance(k, str) or not isinstance(v, str)
                                          for k, v in cache.items()):
        raise RuntimeError("BUILD_SOURCE_CACHE must be a JSON object of repository URLs and paths")
    target = Path(destination).absolute()
    if os.path.lexists(target):
        raise RuntimeError("Build context destination already exists")
    target.parent.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=".brood-context-", dir=target.parent))
    try:
        reports = {}
        portable = {key: copy.deepcopy(project.plan[key]) for key in
                    ("apiVersion", "project", "environment", "sources", "build", "pins", "pinsFile")}
        for name, spec in project.plan["sources"].items():
            path = staging / spec["destination"]
            if spec["type"] == "project":
                report = sources.snapshot(project.root, spec["revision"], path, spec["gitMetadata"])
            else:
                report = _external(spec, path, cache)
            reports[name] = {**spec, **report}
            portable["sources"][name] = reports[name]
        dockerfile = staging / project.plan["build"]["dockerfile"]
        if not dockerfile.is_file():
            raise RuntimeError("Dockerfile is missing from the selected source commit")
        dockerfile.resolve().relative_to(staging)
        (staging / "build-plan.json").write_text(json.dumps(portable, indent=2, sort_keys=True) + "\n")
        staging.rename(target)
        return reports
    finally:
        if staging.exists():
            shutil.rmtree(staging)
