"""brood substrate uninstall: delete the cluster, its registry and the cluster data

Removes the ownership records and the kubeconfig too. The downloaded kubectl,
bin/kubectl-ate and the build caches stay on disk.
"""
from brood.dev.substrate.cli._common import cluster, no_arguments


def main(argv):
    no_arguments("uninstall", argv)
    cluster().uninstall()
    print("Substrate uninstalled; its cluster data is deleted")
