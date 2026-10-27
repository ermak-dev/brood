"""brood build [TARGET]: build an OCI image and Substrate manifests from Broodfile

Reads committed sources, never working-tree overlays. --plan prints the resolved
plan; --export prepares a portable context without Docker. Building publishes an
image to the owned installation registry, but does not register templates or run
actors. Installation context: BUILD_REGISTRY, BUILD_ATESPACE, BUILD_SOURCE_CACHE.
"""

import argparse
import json
from pathlib import Path
import sys

from brood.common import config
from brood.dev.build import artifacts, backend, context, project
from brood.dev.substrate.cli._common import cluster


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("target", nargs="?")
    parser.add_argument("--project", default=".")
    parser.add_argument("--ref", default="HEAD")
    parser.add_argument("--registry")
    parser.add_argument("--atespace")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--plan", action="store_true")
    mode.add_argument("--export", metavar="PATH")
    args = parser.parse_args(argv)
    selected = project.load(args.project, args.ref, args.target,
                            args.registry or config.get("BUILD_REGISTRY"),
                            args.atespace or config.get("BUILD_ATESPACE"))
    if selected.dirty:
        print("Working-tree changes are excluded; building the selected Git commit.", file=sys.stderr)
    if args.plan:
        print(json.dumps(selected.plan, indent=2, sort_keys=True))
        return 0
    try:
        cache = json.loads(config.get("BUILD_SOURCE_CACHE") or "{}")
    except ValueError:
        raise RuntimeError("BUILD_SOURCE_CACHE must contain a JSON object") from None
    if args.export:
        context.export(selected, args.export, cache)
        print(f"Build context prepared at {Path(args.export).absolute()}")
        return 0
    try:
        result = backend.build(selected, cluster(), cache)
    except backend.BuildError as error:
        print(error.stdout, end="", file=sys.stderr)
        print(error.stderr, end="", file=sys.stderr)
        raise
    print(result.stdout, end="", file=sys.stderr)
    print(result.stderr, end="", file=sys.stderr)
    outputs = artifacts.render(selected.plan, result.image, result.sources)
    path = artifacts.publish(selected.root, selected.plan["target"], outputs)
    print(f"Image: {result.image}")
    print(f"Artifacts: {path}")
    return 0
