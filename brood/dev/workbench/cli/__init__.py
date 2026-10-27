"""isolated Brood build, test and test-deploy environments

  bro build                   Go client: build image and manifests from committed Broodfile
  brood workbench build       compatibility interface for image-only builds
  brood workbench template    print an ActorTemplate, development pool or egress policy
  brood workbench start       initialize the workspace and Docker inside the image
  brood workbench run         create a new actor, allow build egress, then start it
  brood workbench prepare     guest-only infrastructure for a private Go endpoint

The image has no host Docker socket, kubeconfig or installation secrets. Each
actor uses its own Docker daemon and a kind cluster inside its microVM. The
workspace is durable; the guest root filesystem is not the Docker data store.
"""
import sys


def main(argv):
    sys.exit(__doc__.strip())
