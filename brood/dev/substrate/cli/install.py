"""brood substrate install: build Substrate from the checkout and deploy it to kind

Creates the registry substrate-registry and the kind cluster substrate when
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
    c = cluster()
    c.install()
    print(ready(c))
