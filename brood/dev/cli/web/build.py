"""brood dev web build: build React source into the Go server's embedded bundle."""
from brood.dev.cli._common import usage
from brood.dev.cli.web._common import run


def main(argv):
    if argv:
        usage(__doc__)
    return run("build")
