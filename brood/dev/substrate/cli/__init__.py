"""the local Substrate: a dedicated kind cluster built from a Substrate checkout

  brood substrate install    build Substrate from the checkout and deploy it; again to redeploy
  brood substrate start      start a stopped installation and wait until it is ready
  brood substrate stop       stop the containers, keep the cluster data
  brood substrate uninstall  delete the cluster, its registry and its data

The checkout is SUBSTRATE_SOURCE in .env. The cluster is kind cluster
`substrate` with registry `substrate-registry` on 127.0.0.1:5002; their
kubectl and kubeconfig live in <checkout>/bin/substrate, and the current
kubectl context is left alone. Needs Docker with IPv6 in its kernel, and Go
for install. See docs/SUBSTRATE.md.
"""
import sys


def main(argv):
    sys.exit(__doc__.strip())
