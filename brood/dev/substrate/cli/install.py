"""brood substrate install: build Substrate from the checkout and deploy it to kind

Without SUBSTRATE_SOURCE, first clones github.com/agent-substrate/substrate
into $XDG_DATA_HOME/brood/substrate, or moves that clone to the commit pinned
in brood; a clone with local changes or another origin is refused. Creates
the registry substrate-registry and the kind cluster substrate when
they are missing, downloads kubectl into <checkout>/bin/substrate, deploys
the checkout with gVisor and the envoy dataplane into ate-system, builds
<checkout>/bin/kubectl-ate and waits until everything is ready. Run it again
to redeploy changes of the checkout into the same cluster.

The ingress router permits the ate-system/ate-client ServiceAccount identity;
the bundled credential provider starts with its default-deny namespace policy.
Refuses to touch containers with these names that this installation does not
own, and a Docker kernel without IPv6.
"""
from brood.dev.substrate.cli._common import cluster, no_arguments, ready


def main(argv):
    no_arguments("install", argv)
    c = cluster(fetch=True)
    c.install()
    print(ready(c))
