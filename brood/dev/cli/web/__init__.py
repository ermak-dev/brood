"""Build and test the React panel embedded in the Go server.

  brood dev web build  rebuild the embedded release bundle
  brood dev web test   run tests, lint, type checks and production build
"""
from brood.dev.cli._common import usage


def main(argv):
    usage(__doc__)
