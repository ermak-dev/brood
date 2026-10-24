"""brood dev web build: build the panel into brood/server/web/dist/index.html

The single-file build the edge image ships; the image builds it again from
source, so dist/ is not committed.
"""
from brood.dev.cli._common import usage
from brood.dev.cli.web._common import run


def main(argv):
    if argv:
        usage(__doc__)
    return run("build")
