"""Чтение декларации и pins из выбранного Git commit проекта."""

from dataclasses import dataclass
import json
from pathlib import Path

from brood.dev.build import plan, sources


@dataclass(frozen=True)
class Project:
    root: Path
    revision: str
    dirty: bool
    plan: dict


def _pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def load(path, ref="HEAD", target=None, registry=None, atespace=None):
    """Не читает рабочие Broodfile и pins, даже если они изменены."""
    root = Path(sources.git(Path(path).resolve(), "rev-parse", "--show-toplevel").strip())
    sha = sources.revision(root, ref)
    document = plan.parse_document(sources.git(root, "show", f"{sha}:Broodfile"))
    pins = None
    if "pinsFile" in document:
        name = plan.safe_path(document["pinsFile"], "pinsFile")
        text = sources.git(root, "show", f"{sha}:{name}")
        if len(text.encode()) > 1024 * 1024:
            raise RuntimeError("pinsFile exceeds 1 MiB")
        try:
            pins = json.loads(text, object_pairs_hook=_pairs)
        except (ValueError, RecursionError):
            raise RuntimeError("pinsFile must contain a JSON object of string pins") from None
    resolved = plan.make_plan(document, pins, target, registry, atespace)
    for source in resolved["sources"].values():
        if source["type"] == "project":
            source["revision"] = sha
            remote = sources.origin(root, required=source["gitMetadata"])
            if remote is not None:
                source["repository"] = remote
    dirty = bool(sources.git(root, "status", "--porcelain", "-z"))
    return Project(root, sha, dirty, resolved)
