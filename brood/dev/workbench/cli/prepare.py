"""brood workbench prepare: prepare owned infrastructure and a pinned worker for the private Go endpoint.

Only available inside a workbench guest after its isolated Substrate cluster is
started. Does not create users, switch public traffic or consume host credentials.
"""
import os

from brood.dev.substrate.cli._common import cluster
from brood.dev.workbench import runtime
from brood.dev.workbench.prepare import prepare


def main(argv):
    if argv:
        raise RuntimeError("workbench prepare takes no arguments")
    runtime.check_environment(os.environ)
    root = os.environ.get("WORKBENCH_ROOT")
    if not root or not os.path.isabs(root):
        raise RuntimeError("workbench prepare requires an absolute WORKBENCH_ROOT")
    images = prepare(cluster(), root, os.environ)
    print(f"Guest Go endpoint infrastructure prepared with worker {images['gvisor']}")
    return 0
