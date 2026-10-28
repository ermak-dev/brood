"""brood deploy lxc --trusted-workloads: prepare the experimental LXC runtime

Requires the installed verified Substrate fork and the owned brood namespace.
Builds the fork's LXC worker image, pushes its immutable registry digest and
prepares the explicit lxc-image SandboxConfig. Does not change worker pools,
stop actors, change a global class default, or upgrade the control plane.

LXC uses a shared node kernel. Trusted workloads only: no memory checkpoint or
automatic process recovery after node failure. --trusted-workloads acknowledges
these limits. No host, PVE or VM settings are changed.
"""
from brood.dev.deploy import lxc
from brood.dev.substrate.cli._common import cluster


def main(argv):
    if argv in (["--help"], ["-h"]):
        print(__doc__.strip())
        return 0
    if argv != ["--trusted-workloads"]:
        raise RuntimeError("brood deploy lxc requires --trusted-workloads and no other arguments: "
                           "LXC shares the node kernel and is for trusted workloads only")
    image = lxc.Deploy(cluster()).prepare()
    print(f"LXC worker image prepared: {image}")
    print("SandboxConfig lxc-image prepared; no worker pools or actors were changed")
    print("Trusted workloads only: shared node kernel, no memory checkpoint or automatic recovery")
    return 0
