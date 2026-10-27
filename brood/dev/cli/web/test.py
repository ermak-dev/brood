"""brood dev web test: run panel tests, lint and build with the pinned Bun image."""
from brood.dev.cli._common import usage
from brood.dev.cli.web._common import run


def main(argv):
    if argv:
        usage(__doc__)
    return run("verify")
