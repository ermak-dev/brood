"""brood deploy go: build and prepare the private Go server endpoint

Requires BROOD_SESSION_SECRET, approved WORKER_POOL_IMAGES and TLS files.
Uses the existing owned installation and a separate PVC. Does not import users,
create an administrator, remove existing resources, or change public ingress.
This preparation step is not a completed public deployment.
"""

from brood.dev.deploy.go_server import Deploy
from brood.dev.substrate.cli._common import cluster


def main(argv):
    if argv:
        raise RuntimeError("brood deploy go takes no arguments")
    image = Deploy(cluster()).run()
    print(f"Go endpoint prepared: {image}")
    print("Public ingress is unchanged. Use brood deploy server to bootstrap admin and publish the endpoint.")
    return 0
