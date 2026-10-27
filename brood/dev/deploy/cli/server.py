"""brood deploy server: publish the Go server and bootstrap its administrator

Requires fresh Go settings and TLS files in the release. Existing actor
infrastructure and worker pools are preserved. --check performs preflight only.
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
    print(f"Administrator ready: {result['admin']}")
    return 0
