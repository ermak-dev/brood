"""Совместимость workbench build с Go-компилятором Broodfile."""

from pathlib import Path
import subprocess

from brood.common import config


HERE = Path(__file__).resolve().parent


class Builder:
    """Go-клиент владеет экспортом, сборкой и публикацией артефактов."""

    def __init__(self, project=config.PROJECT, client="bro"):
        self.project, self.client = Path(project), client

    def _run(self, ref, extra=()):
        args = [self.client, "build", "development", "--project", str(self.project), "--ref", ref]
        for flag, key in (("--registry", "BUILD_REGISTRY"), ("--atespace", "BUILD_ATESPACE")):
            if value := config.get(key):
                args.extend([flag, value])
        try:
            result = subprocess.run([*args, *extra], capture_output=True, text=True)
        except FileNotFoundError:
            raise RuntimeError(f"{self.client} is not installed; install the Go bro client") from None
        if result.returncode:
            raise RuntimeError(f"Go workbench build failed: {(result.stderr or result.stdout).strip()}")
        return result.stdout.strip()

    def context(self, directory, ref="HEAD"):
        directory = Path(directory).resolve()
        if directory == HERE.resolve() or HERE.resolve() in directory.parents:
            raise RuntimeError("build context must be outside the workbench source directory")
        self._run(ref, ("--export", str(directory)))

    def build(self, ref="HEAD"):
        # Вторую строку Go CLI занимает путь опубликованных build artifacts.
        lines = self._run(ref).splitlines()
        if not lines:
            raise RuntimeError("Go workbench build returned no image")
        return lines[0]
