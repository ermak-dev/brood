"""brood workbench build [--export PATH]: build or export the current development image

Uses current working files and the pinned Substrate revision. Local .env,
secrets and Git credentials are not copied. The OCI image and template are
saved locally; bro push publishes them through a logged-in server session.
The Go bro client must be installed on PATH. amd64 only.
"""
import argparse
from pathlib import Path

from brood.dev.workbench.build import Builder


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--export", help="prepare a clean context in a new directory without building")
    args = parser.parse_args(argv)
    builder = Builder()
    if args.export:
        path = Path(args.export)
        if path.exists():
            raise RuntimeError("workbench export directory already exists; choose a new path")
        builder.context(args.export)
        print(f"Workbench build context prepared at {path}")
    else:
        print(builder.build())
    return 0
