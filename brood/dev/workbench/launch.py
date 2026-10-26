"""Запуск нового build-актора после установки его явной политики сети."""
import json
import tempfile
from pathlib import Path

from brood.dev.substrate import local
from brood.dev.workbench import manifest


class Launcher:
    """Control plane берётся только из проверенной локальной установки."""

    def __init__(self, cluster):
        self.cluster = cluster

    def start(self, atespace, template, name, extra_hosts=()):
        policy = manifest.egress_policy(atespace, name, manifest.PUBLIC_HOSTS + tuple(extra_hosts))
        manifest._dns_label(template)
        self.cluster.check()
        source = self._ate("get", "actor-template", template, "-a", atespace)
        if source.get("workerSelector", {}).get("matchLabels") != {
                "brood.dev/purpose": "development"}:
            raise RuntimeError("workbench requires a template with the development worker selector")
        self._ate("create", "actor", name, "-a", atespace, "--template", template)
        with tempfile.TemporaryDirectory(prefix="brood-workbench-policy.") as directory:
            path = Path(directory) / "policy.json"
            path.write_text(json.dumps(policy))
            try:
                self._ate("create", "egress-policy", name, "-a", atespace, "-f", str(path))
            except RuntimeError as error:
                raise RuntimeError(f"new actor {name} remains suspended: {error}") from None
        return self._ate("resume", "actor", name, "-a", atespace)

    def _ate(self, *args):
        code, out, err = self.cluster.shell.output(
            self.cluster.kubectl_ate, f"--context={local.CONTEXT}", *args, "-o", "json")
        if code:
            reason = "\n".join(line for line in err.splitlines() if not line.startswith("Warning:"))
            raise RuntimeError(local.reason(reason, code))
        try:
            result = json.loads(out)
            if not isinstance(result, dict):
                raise ValueError("expected an object")
            return result
        except ValueError as error:
            raise RuntimeError(f"invalid ateapi response: {error}") from None
