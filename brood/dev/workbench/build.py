"""Совместимость workbench build с общей сборкой из Broodfile."""

from pathlib import Path

from brood.common import config
from brood.dev.build import backend, context, project, sources
from brood.dev.substrate import local


HERE = Path(__file__).resolve().parent


def snapshot(source, ref, target):
    """Старый интерфейс снимка сохраняет настоящий Git commit и безопасный origin."""
    try:
        return sources.snapshot(source, ref, target, git_metadata=True)["revision"]
    except RuntimeError as error:
        if str(error).startswith("repository:"):
            raise RuntimeError("source origin must not contain credentials or unsafe URLs") from None
        raise


class Builder:
    """Старый командлет использует тот же план, контекст и backend, что brood build."""

    def __init__(self, cluster, project=config.PROJECT):
        self.cluster, self.project = cluster, Path(project)

    def _load(self, ref):
        return project.load(self.project, ref, "development", config.get("BUILD_REGISTRY"),
                            config.get("BUILD_ATESPACE"))

    def _cache(self):
        return {local.REPOSITORY: str(self.cluster.source)}

    def context(self, directory, ref="HEAD"):
        directory = Path(directory).resolve()
        if directory == HERE.resolve() or HERE.resolve() in directory.parents:
            raise RuntimeError("build context must be outside the workbench source directory")
        selected = self._load(ref)
        context.export(selected, directory, self._cache())
        return selected.revision

    def build(self, ref="HEAD"):
        return backend.build(self._load(ref), self.cluster, self._cache()).image
