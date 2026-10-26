"""brood workbench run --atespace NAME --template NAME --name NAME [--allow-host HOST]

Create a new actor from a development template, create its explicit egress
policy, and only then resume it. An existing actor is never overwritten. A
policy failure leaves the newly created actor suspended for inspection.
"""
import argparse
import json

from brood.dev.substrate.cli._common import cluster
from brood.dev.workbench.launch import Launcher


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--atespace", required=True)
    parser.add_argument("--template", required=True)
    parser.add_argument("--name", required=True)
    parser.add_argument("--allow-host", action="append", default=[])
    args = parser.parse_args(argv)
    try:
        result = Launcher(cluster()).start(args.atespace, args.template, args.name, args.allow_host)
    except ValueError as error:
        raise RuntimeError(str(error)) from None
    print(json.dumps(result, indent=2))
    return 0
