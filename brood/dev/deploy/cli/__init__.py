"""Go server deployment tooling for the local Substrate installation

  brood deploy server    publish Go, bootstrap admin and retire Django/Postgres
  brood deploy go        build and prepare the private Go endpoint
  brood deploy microvm   switch an empty installation to a microVM worker pool

Go deployment requires session/admin settings, approved WORKER_POOL_IMAGES,
and TLS files. No legacy users are imported. Public rollout verifies admin
access before deleting obsolete server data; shared actor infrastructure remains.
"""
import sys


def main(argv):
    sys.exit(__doc__.strip())
