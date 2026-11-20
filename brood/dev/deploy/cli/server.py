"""brood deploy server: publish the Go server with TLS

Initial mode bootstraps an administrator and verifies login.
BROOD_DEPLOY_MODE=fixture (temporary) recreates the database on every release
and restores the administrator and Telegram from secrets/fixture.json. Explicit
BROOD_DEPLOY_MODE=existing snapshots the owned database without credentials
and has the new image verify that it opens or migrates the snapshot before
apply; operator-authenticated acceptance remains pending. --check is read-only.
Existing actor infrastructure and worker pools are preserved.

Disk guard (docs/DEPLOY.md): BROOD_DEPLOY_MIN_FREE_GB refuses the deploy
before the build when the Docker partition has less free space;
BROOD_DEPLOY_BUILD_CACHE_GB removes old brood-go/edge image tags and prunes
the build cache after a successful deploy; BROOD_DEPLOY_KEEP_SNAPSHOTS keeps
that many database snapshots in existing mode. Unset, each step is skipped
with a warning.
"""
from brood.common import config
from brood.dev.deploy import disk
from brood.dev.deploy.public import Deployment, settings
from brood.dev.substrate.cli._common import cluster


def space_line(space):
    if not space.get("checked"):
        return f"warning: free space check skipped: {disk.MIN_FREE} is not set (docs/DEPLOY.md)"
    return (f"Free space: {disk.gib(space['free'])} on {space['mount']} (Docker root {space['root']}), "
            f"required {disk.gib(space['limit'])}")


def docker_lines(docker):
    if not docker.get("enabled"):
        return [f"warning: Docker cleanup skipped: {disk.BUILD_CACHE} is not set; the build cache and old "
                "release images keep growing (docs/DEPLOY.md)"]
    lines = [f"Docker cleanup: removed {len(docker['removed'])} old release image tags, kept "
             f"{', '.join(docker['kept']) or 'none'}; build cache pruned to {docker['cache']} GiB, "
             f"reclaimed {docker['reclaimed'] or 'unknown'}"]
    return lines + [f"warning: Docker cleanup failed: {error}" for error in docker["errors"]]


def snapshot_line(snapshots):
    if not snapshots.get("enabled"):
        return (f"warning: snapshot retention skipped: {disk.KEEP_SNAPSHOTS} is not set; private database "
                "snapshots accumulate on the owned PVC (docs/DEPLOY.md)")
    if "error" in snapshots:
        return (f"warning: snapshot retention failed: {snapshots['error']}; "
                "older snapshots may remain on the owned PVC")
    return (f"Snapshot retention: kept {snapshots['kept']} of {disk.KEEP_SNAPSHOTS}={snapshots['keep']} "
            f"including this deploy's snapshot, removed {snapshots['removed']}")


def schema_line(schema):
    if schema["from"] == schema["to"]:
        return f"Database schema {schema['to']}: no migration"
    return (f"Database schema migrated from {schema['from']} to {schema['to']} on server start; "
            "rollback restores the snapshot above with the previous image (docs/DEPLOY.md)")


def main(argv):
    if argv not in ([], ["--check"]):
        raise RuntimeError("brood deploy server accepts only --check")
    settings(config.get)
    deployment = Deployment(cluster())
    if argv:
        space = deployment.space()
        deployment.check()
        print(space_line(space))
        print("Public Go deployment preflight passed")
        return 0
    result = deployment.run()
    if "space" in result:
        print(space_line(result["space"]))
    print(f"Go server deployed: {result['image']}")
    print(f"Public endpoint: https://{result['host']}")
    if "acceptance" in result:
        if result.get("recovered"):
            print("Recovered an interrupted rollout: crash-looping pod replaced after verified snapshot")
        print(f"Rollout passed; {result['acceptance']}")
        print(f"Private database snapshot retained on owned PVC: {result['snapshot']}")
        if result.get("schema"):
            print(schema_line(result["schema"]))
        if "snapshots" in result:
            print(snapshot_line(result["snapshots"]))
    elif result.get("fixture"):
        print("Database recreated for this release; administrator and Telegram restored from the fixture")
    else:
        print(f"Administrator ready: {result['admin']}")
    if "docker" in result:
        print("\n".join(docker_lines(result["docker"])))
    return 0
