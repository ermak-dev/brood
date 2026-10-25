"""brood deploy server: build the brood images and deploy the server, CLIProxyAPI and the edge into the local Substrate cluster

Builds the server image (brood/dev/deploy/Dockerfile) and the edge image
with the panel (brood/dev/deploy/edge/Dockerfile) from the working copy,
pushes them to the installation's registry and applies
brood/dev/deploy/manifests with the images pinned by digest and Secrets made
from .env and secrets/. Builds the gvisor worker image from the installation's
Substrate checkout using its pinned ko tool and creates the initial brood pool
only if absent; existing pool settings and versions are kept. Waits for the
pool's generated Deployment, Postgres, Django, CLIProxyAPI and the edge to roll
out. With BROOD_HTTPS_PORT set, then checks over https on that host
port, with the deployed certificate, that the panel, the admin login, the
management gate and the LLM API answer.
"""
from brood.dev.deploy import server
from brood.dev.substrate.cli._common import cluster


def main(argv):
    if argv:
        raise RuntimeError("brood deploy server takes no arguments")
    digests, url = server.Deploy(cluster()).server()
    for name, digest in sorted(digests.items()):
        print(f"{name} {server.REGISTRY}/{name}@{digest}")
    if url:
        print(f"brood serves {url}")
    else:
        print("brood is deployed; BROOD_HTTPS_PORT is not set, so reach it with "
              "kubectl port-forward -n brood svc/edge 8443:443")
    return 0
