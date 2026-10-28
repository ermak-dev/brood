"""the local Substrate: a dedicated kind cluster built from a Substrate checkout

  brood substrate install    build Substrate from the checkout and deploy it; again to redeploy
  brood substrate start      start a stopped installation and wait until it is ready
  brood substrate stop       stop the containers, keep the cluster data
  brood substrate uninstall  delete the cluster, its registry and its data

The checkout is SUBSTRATE_SOURCE in .env, a developer's clone, taken as it
is. Without it install keeps its own clone of the verified LXC fork at the
commit pinned in brood, in $XDG_DATA_HOME/brood/substrate (by default
~/.local/share/brood/substrate). The cluster is kind cluster
`substrate` with registry `substrate-registry` on 127.0.0.1:5002; their
kubectl and kubeconfig live in <checkout>/bin/substrate, and the current
kubectl context is left alone. Needs Docker with IPv6 in its kernel, and Go
for install. See docs/SUBSTRATE.md.
"""
import sys


def main(argv):
    sys.exit(__doc__.strip())
