"""brood deploy server: build the server image and deploy it with Postgres into the local Substrate cluster

Builds brood/dev/deploy/Dockerfile from the working copy, pushes it to the
installation's registry and applies brood/dev/deploy/manifests with the
image pinned by digest and Secrets made from .env and secrets/. Waits for
Postgres and the server to roll out. With BROOD_HTTPS_PORT set, then checks
that the admin login page answers over https on that host port with the
deployed certificate.
"""
from brood.dev.deploy import server
from brood.dev.substrate.cli._common import cluster


def main(argv):
    if argv:
        raise RuntimeError("brood deploy server takes no arguments")
    ref, url = server.Deploy(cluster()).server()
    if url:
        print(f"brood server {ref} serves {url}")
    else:
        print(f"brood server {ref} is deployed; BROOD_HTTPS_PORT is not set, so reach it with "
              f"kubectl port-forward -n brood svc/brood-server 8443:443")
    return 0
