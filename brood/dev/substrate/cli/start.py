"""brood substrate start: start a stopped installation and wait until it is ready

Starts the registry and the cluster node, then waits for the Kubernetes API,
the node, every deployment in ate-system and the atelet daemonset.
"""
from brood.dev.substrate.cli._common import cluster, no_arguments, ready


def main(argv):
    no_arguments("start", argv)
    c = cluster()
    c.start()
    print(ready(c))
