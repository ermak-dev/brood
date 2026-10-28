"""Совместимость workbench build с Go-компилятором Broodfile."""

from pathlib import Path
import subprocess

from brood.common import config


HERE = Path(__file__).resolve().parent


class Builder:
    """Go-клиент владеет экспортом, сборкой и публикацией артефактов."""

    def __init__(self, project=config.PROJECT, client="bro"):
        self.project, self.client = Path(project), client

    def _run(self, extra=()):
        args = [self.client, "build", "development"]
        if value := config.get("BUILD_PROJECT"):
            args.extend(["--project", value])
        try:
            result = subprocess.run([*args, *extra], cwd=self.project, capture_output=True, text=True)
        except FileNotFoundError:
            raise RuntimeError(f"{self.client} is not installed; install the Go bro client") from None
        if result.returncode:
            raise RuntimeError(f"Go workbench build failed: {(result.stderr or result.stdout).strip()}")
        return result.stdout.strip()

    def context(self, directory):
        directory = Path(directory).resolve()
        if directory == HERE.resolve() or HERE.resolve() in directory.parents:
            raise RuntimeError("build context must be outside the workbench source directory")
        self._run(("--export", str(directory)))

    def build(self):
        # Вторую строку Go CLI занимает путь локальных build artifacts.
        lines = self._run().splitlines()
        if not lines:
            raise RuntimeError("Go workbench build returned no image")
        return lines[0]
