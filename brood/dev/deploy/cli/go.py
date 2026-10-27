"""brood deploy go: deploy the Go server alongside Django, without switching traffic

Requires BROOD_SESSION_SECRET in .env and secrets/tls.crt + secrets/tls.key.
Uses the existing owned installation and a separate PVC. Does not import users,
create an administrator, remove Django, or change the public ingress.
"""

from brood.common import config
from brood.dev.deploy.go_server import Deploy
from brood.dev.substrate.cli._common import cluster


def main(argv):
    if argv == ["--if-enabled"]:
        enabled = config.get("BROOD_GO_DEPLOY")
        if enabled not in ("", "0", "1"):
            raise RuntimeError("BROOD_GO_DEPLOY must be 0 or 1")
        if enabled != "1":
            return 0
    elif argv:
        raise RuntimeError("brood deploy go accepts only --if-enabled")
    image = Deploy(cluster()).run()
    print(f"Go server deployed: {image}")
    print("Django ingress is unchanged. Bootstrap or migrate users before switching traffic.")
    return 0
