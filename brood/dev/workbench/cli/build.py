"""brood workbench build [--ref COMMIT] [--export PATH]: build or export the development image

Uses the current working copy's committed source and the pinned Substrate
revision. Local .env, secrets and Git credentials are not copied. The image is
published to the owned installation registry and printed by digest. amd64 only.
"""
import argparse
from pathlib import Path

from brood.dev.substrate.cli._common import cluster
from brood.dev.workbench.build import Builder


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ref", default="HEAD")
    parser.add_argument("--export", help="prepare a clean context in a new directory without building")
    args = parser.parse_args(argv)
    builder = Builder(cluster())
    if args.export:
        path = Path(args.export)
        if path.exists():
            raise RuntimeError("workbench export directory already exists; choose a new path")
        builder.context(args.export, args.ref)
        print(f"Workbench build context prepared at {path}")
    else:
        print(builder.build(args.ref))
    return 0
