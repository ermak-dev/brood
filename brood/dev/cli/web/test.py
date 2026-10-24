"""brood dev web test: the panel's checks, tests, lint and build (bun run verify)"""
from brood.dev.cli._common import usage
from brood.dev.cli.web._common import run


def main(argv):
    if argv:
        usage(__doc__)
    return run("verify")
