"""brood workbench template: print development manifests as JSON

  --image DIGEST --atespace NAME                ActorTemplate
  --kind pool --image WORKER_DIGEST              dedicated microVM WorkerPool
  --kind policy --atespace NAME --actor NAME     per-actor egress policy

Creating a template does not create an egress policy for its actors. Apply the
policy to each new actor before resuming it. Only explicit public build hosts
are allowed; neither the host Docker API nor the production cluster is exposed.
"""
import argparse
import json

from brood.dev.workbench import manifest


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kind", choices=("template", "pool", "policy"), default="template")
    parser.add_argument("--image")
    parser.add_argument("--atespace")
    parser.add_argument("--name", default="brood-build-test-deploy")
    parser.add_argument("--actor")
    parser.add_argument("--allow-host", action="append", default=[])
    args = parser.parse_args(argv)
    try:
        if args.kind == "pool":
            result = manifest.worker_pool(args.image)
        elif args.kind == "policy":
            result = manifest.egress_policy(args.atespace, args.actor,
                                             manifest.PUBLIC_HOSTS + tuple(args.allow_host))
        else:
            result = manifest.actor_template(args.image, args.atespace, args.name)
    except ValueError as error:
        raise RuntimeError(str(error)) from None
    print(json.dumps(result, indent=2))
    return 0
