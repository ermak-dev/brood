"""brood deploy server: publish the Go server with TLS

Initial mode bootstraps an administrator and verifies login. Explicit
BROOD_DEPLOY_MODE=existing snapshots the owned database without credentials;
operator-authenticated acceptance remains pending. --check is read-only.
Existing actor infrastructure and worker pools are preserved.
"""
from brood.common import config
from brood.dev.deploy.public import Deployment, settings
from brood.dev.substrate.cli._common import cluster


def main(argv):
    if argv not in ([], ["--check"]):
        raise RuntimeError("brood deploy server accepts only --check")
    settings(config.get)
    deployment = Deployment(cluster())
    if argv:
        deployment.check()
        print("Public Go deployment preflight passed")
        return 0
    result = deployment.run()
    print(f"Go server deployed: {result['image']}")
    print(f"Public endpoint: https://{result['host']}")
    if "acceptance" in result:
        if result.get("recovered"):
            print("Recovered an interrupted rollout: crash-looping pod replaced after verified snapshot")
        print(f"Rollout passed; {result['acceptance']}")
        print(f"Private database snapshot retained on owned PVC: {result['snapshot']}")
    else:
        print(f"Administrator ready: {result['admin']}")
    return 0
