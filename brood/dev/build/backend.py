"""Docker BuildKit: публикация в проверенный реестр без запуска акторов."""

from dataclasses import dataclass
import json
from pathlib import Path
import re
import subprocess
import tempfile
import uuid

from brood.dev.build import context
from brood.dev.substrate import local


@dataclass(frozen=True)
class Result:
    image: str
    sources: dict
    stdout: str
    stderr: str


class BuildError(RuntimeError):
    """Диагностика процесса остаётся данными для командлета."""

    def __init__(self, result):
        super().__init__(f"OCI image build failed with exit code {result.returncode}")
        self.stdout, self.stderr = result.stdout, result.stderr


def build(project, cluster, cache=None, runner=subprocess.run):
    """Не разрешает mutable tag после push: digest принадлежит metadata этого вызова."""
    plan = project.plan
    registry = plan["registry"]
    if registry.split("/", 1)[0] != f"localhost:{local.REGISTRY_PORT}":
        raise RuntimeError("BUILD_REGISTRY must use the owned installation registry")
    cluster.check()
    with tempfile.TemporaryDirectory(prefix="brood-build-") as directory:
        root = Path(directory)
        source = root / "context"
        reports = context.export(project, source, cache)
        metadata = root / "image-metadata.json"
        image = f"{registry}/{plan['project']}/{plan['environment']}"
        tag = image + ":build-" + uuid.uuid4().hex
        argv = ["docker", "buildx", "build", "--push", "--platform", plan["build"]["platform"],
                "--file", str(source / plan["build"]["dockerfile"]), "--tag", tag,
                "--metadata-file", str(metadata)]
        for name, value in sorted(plan["build"]["arguments"].items()):
            argv.extend(["--build-arg", f"{name}={value}"])
        argv.append(str(source))
        try:
            result = runner(argv, cwd=source, env=cluster.shell.env, capture_output=True, text=True)
        except FileNotFoundError:
            raise RuntimeError("docker is not installed") from None
        if result.returncode:
            raise BuildError(result)
        try:
            digest = json.loads(metadata.read_text())["containerimage.digest"]
        except (OSError, ValueError, KeyError, TypeError):
            raise RuntimeError("BuildKit returned no usable image metadata") from None
        if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
            raise RuntimeError("BuildKit metadata contains an invalid image digest")
        return Result(image + "@" + digest, reports, result.stdout, result.stderr)
