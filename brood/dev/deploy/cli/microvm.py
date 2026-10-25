"""brood deploy microvm: switch the empty installation to a microVM worker pool

Checks installation ownership, KVM and the absence of actors. Installs the pinned
Substrate microVM assets and builds its worker image. Creates brood-microvm with
brood's worker settings and waits for its runtime registration. Drain the old
gvisor workers through the panel, then rerun to scale brood to zero without
deleting it. Existing brood-microvm pools are reused
only when they belong to this migration. A failure does not delete either pool;
concurrent updates to brood are refused rather than overwritten.

This changes the runtime, not the actor image or its Docker permissions. The
panel's microVM editing support is separate; this command does not change it.
"""
from brood.dev.deploy import microvm
from brood.dev.substrate.cli._common import cluster


def main(argv):
    if argv:
        raise RuntimeError("brood deploy microvm takes no arguments")
    image = microvm.Deploy(cluster()).migrate()
    print(f"microVM pool brood/brood-microvm is ready: {image}")
    print("gvisor pool brood/brood is stopped and retained for rollback")
    return 0
