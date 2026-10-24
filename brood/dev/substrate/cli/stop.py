"""brood substrate stop: stop the cluster node and the registry, keep the cluster data

The cluster storage is kept; the memory of running actors is not snapshotted.
"""
from brood.dev.substrate.cli._common import cluster, no_arguments


def main(argv):
    no_arguments("stop", argv)
    cluster().stop()
    print("Substrate stopped; brood substrate start brings it back")
