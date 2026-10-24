"""the brood panel (brood/server/web): build and check it in the Bun container of the edge image

  brood dev web build    build brood/server/web/dist/index.html
  brood dev web test     the panel's checks: tests, lint and build (bun run verify)

Bun runs in the image the edge Dockerfile builds the panel with
(brood/dev/deploy/edge/Dockerfile), so the host needs only Docker. The panel
is a fork of Cli-Proxy-API-Management-Center kept as a git subtree; see
docs/WEB.md.
"""
import sys


def main(argv):
    sys.exit(__doc__.strip())
