"""Независимые манифесты сборки и атомарная публикация полного комплекта."""

from contextlib import suppress
import copy
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import uuid

from brood.dev.build.plan import repository


_OWNER = "brood.dev/build/v1"
_FILES = frozenset({"actor-template.json", "egress-policy.json", "build.json"})


def _label(value):
    if not isinstance(value, str) or not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", value):
        raise RuntimeError("artifact identity must be a lowercase DNS label of at most 63 characters")
    return value


def _json(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True, allow_nan=False)


def _sources(plan, reports):
    if not isinstance(reports, dict) or reports.keys() - plan["sources"].keys():
        raise RuntimeError("source reports must name selected sources")
    result = {}
    for name, source in plan["sources"].items():
        # Копируются только декларативные поля, никогда пути checkout или config.
        item = {key: copy.deepcopy(source[key]) for key in
                ("type", "destination", "gitMetadata", "repository", "revision") if key in source}
        report = reports.get(name, {})
        if not isinstance(report, dict):
            raise RuntimeError("source report must be an object")
        for key in ("repository", "revision"):
            if key in report:
                if key in item and report[key] != item[key]:
                    raise RuntimeError("source report does not match planned input")
                item[key] = report[key]
        if "repository" in item:
            repository(item["repository"])
        if "revision" in item and (not isinstance(item["revision"], str)
                                   or not re.fullmatch(r"[0-9a-f]{40}", item["revision"])):
            raise RuntimeError("source revision must be a full lowercase 40-hex commit")
        result[name] = item
    return result


def render(plan, image, source_reports=None):
    """Возвращает три JSON-объекта без изменения плана и без побочных эффектов."""
    identity = {key: _label(plan[key]) for key in ("project", "target", "environment", "atespace")}
    prefix = f"{plan['registry']}/{plan['project']}/{plan['environment']}@sha256:"
    if (not isinstance(image, str) or not image.startswith(prefix)
            or not re.fullmatch(r"[0-9a-f]{64}", image[len(prefix):])):
        raise RuntimeError("image must match the planned repository with a sha256 digest")
    actor = copy.deepcopy(plan["actor"])
    for value in actor["resources"]["limits"].values():
        if (not isinstance(value, str) or not re.fullmatch(r"[1-9][0-9]*", value)
                or len(value) > 20 or int(value) > 2**64 - 1):
            raise RuntimeError("actor resource must be a positive uint64 string")
    actor["containers"][0]["image"] = image
    actor.setdefault("snapshotConfig", {}).setdefault(
        "goldenSnapshotConfig", {"mode": "GOLDEN_SNAPSHOT_MODE_DISABLED"})
    canonical = _json({"actor": actor, "network": plan["network"], "identity": identity})
    digest = hashlib.sha256(canonical.encode()).hexdigest()
    stem = f"{plan['project']}-{plan['target']}"[:38].rstrip("-")
    name = f"{stem}-{digest[:24]}"
    actor["metadata"] = {"atespace": plan["atespace"], "name": name}
    rules = []
    for source, protocol in (("tls", "tlsPassthrough"), ("http", "http")):
        if source in plan["network"]:
            rule = plan["network"][source]
            rules.append({protocol: {"hostnames": copy.deepcopy(rule["hosts"]),
                                     "ports": {"numbers": copy.deepcopy(rule["ports"])}}})
    provenance = {
        "apiVersion": plan["apiVersion"], "builder": _OWNER, **identity,
        "output": {"owner": _OWNER, "target": plan["target"]},
        "sources": _sources(plan, {} if source_reports is None else source_reports),
        "build": copy.deepcopy(plan["build"]), "pins": copy.deepcopy(plan["pins"]),
        "image": image, "imageDigest": "sha256:" + image[len(prefix):],
        "template": name, "templateDigest": "sha256:" + digest,
    }
    return {"actor-template.json": actor,
            "egress-policy.json": {"metadata": {"atespace": plan["atespace"], "name": "default"},
                                   "rules": rules},
            "build.json": provenance}


def _directory(path, create=False):
    # Не resolve(): он скрыл бы symlink, который требуется отвергнуть.
    for parent in (*reversed(path.parents), path):
        try:
            mode = parent.lstat().st_mode
        except FileNotFoundError:
            if not create:
                raise RuntimeError("artifact parent directory does not exist") from None
            try:
                parent.mkdir()
            except FileExistsError:
                pass
            mode = parent.lstat().st_mode
        if not stat.S_ISDIR(mode):
            raise RuntimeError("artifact parent must be a directory, not a symlink or file")


def _marker(value, target):
    return (isinstance(value, dict) and value.get("builder") == _OWNER
            and value.get("target") == target
            and value.get("output") == {"owner": _OWNER, "target": target})


def _owned(output, runs, target):
    if not output.is_symlink():
        if output.exists():
            raise RuntimeError("refusing foreign artifact output; expected an owned symlink")
        return
    link = Path(os.readlink(output))
    if (link.is_absolute() or len(link.parts) != 3 or link.parts[:2] != (".runs", target)
            or not re.fullmatch(r"run-[0-9a-f]{32}", link.name)):
        raise RuntimeError("refusing foreign artifact output symlink")
    run = runs / link.name
    try:
        if not stat.S_ISDIR(run.lstat().st_mode) or {p.name for p in run.iterdir()} != _FILES:
            raise ValueError
        for name in _FILES:
            if not stat.S_ISREG((run / name).lstat().st_mode):
                raise ValueError
        if not _marker(json.loads((run / "build.json").read_text()), target):
            raise ValueError
    except (OSError, ValueError):
        raise RuntimeError("refusing artifact output without an owned complete run") from None


def _encoded(artifacts, target):
    if (not isinstance(artifacts, dict) or artifacts.keys() != _FILES
            or not all(isinstance(value, dict) for value in artifacts.values())
            or not _marker(artifacts["build.json"], target)):
        raise RuntimeError("expected a complete owned artifact bundle for this target")
    try:
        return {name: _json(value) + "\n" for name, value in artifacts.items()}
    except (TypeError, ValueError, OverflowError, RecursionError):
        raise RuntimeError("artifact bundle must contain valid JSON objects") from None


def publish(project_root, target, artifacts):
    """Публикует новый комплект, сохраняя прежнюю ссылку при любом отказе."""
    _label(target)
    encoded = _encoded(artifacts, target)
    root = Path(project_root).absolute()
    output = root / ".brood" / "build" / target
    runs = output.parent / ".runs" / target
    run = pointer = None
    lock = None
    committed = False
    try:
        _directory(root)
        _directory(output.parent, create=True)
        # Чужой output отвергается до создания служебного каталога цели.
        if not output.is_symlink() and output.exists():
            raise RuntimeError("refusing foreign artifact output")
        _directory(runs, create=True)
        lock = os.open(runs, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        fcntl.flock(lock, fcntl.LOCK_EX)
        _owned(output, runs, target)
        candidate = runs / ("run-" + uuid.uuid4().hex)
        candidate.mkdir()
        run = candidate
        for name, text in encoded.items():
            destination = run / name
            destination.write_text(text, encoding="utf-8")
            with destination.open("rb") as stream:
                os.fsync(stream.fileno())
        with_directory = os.open(run, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(with_directory)
        finally:
            os.close(with_directory)
        # Уникальная временная ссылка не задевает существующие файлы соседей.
        candidate = output.parent / (".pointer-" + uuid.uuid4().hex)
        candidate.symlink_to(Path(".runs") / target / run.name, target_is_directory=True)
        pointer = candidate
        _directory(runs)
        _owned(output, runs, target)
        os.replace(pointer, output)
        committed = True
        return output
    except OSError:
        raise RuntimeError("could not publish artifact bundle; previous output preserved") from None
    finally:
        if not committed:
            # Ошибка уборки не маскирует исходный отказ; удаляем лишь созданное нами.
            if pointer is not None:
                with suppress(OSError):
                    pointer.unlink(missing_ok=True)
            if run is not None:
                with suppress(OSError):
                    shutil.rmtree(run)
        if lock is not None:
            os.close(lock)
