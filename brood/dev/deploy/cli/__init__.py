"""Go server deployment tooling for the local Substrate installation

  brood deploy server    check public Go cutover readiness (currently blocked)
  brood deploy go        build and prepare the private Go endpoint
  brood deploy microvm   switch an empty installation to a microVM worker pool

Go preparation requires BROOD_SESSION_SECRET, approved WORKER_POOL_IMAGES,
secrets/tls.crt and secrets/tls.key. No command imports live users or switches
public traffic automatically. Existing databases, PVCs and actors are retained.
"""
import sys


def main(argv):
    sys.exit(__doc__.strip())
