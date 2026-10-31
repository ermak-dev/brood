"""brood workbench start [--root PATH] [--seeds PATH] [--results-port PORT] -- COMMAND [ARG ...]

The workbench image entrypoint. Copies initial source trees only once, creates
private test settings and TLS, mounts guest cgroups, starts its own Docker
using durable VFS storage, and executes the requested job. The results port
(default 80) serves only workspace/results, not source, credentials or the Docker API.
"""
import argparse
import os
import subprocess
import sys
from pathlib import Path

from brood.dev.workbench import bootstrap, runtime


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=os.environ.get("WORKBENCH_ROOT"))
    parser.add_argument("--seeds", default=os.environ.get("WORKBENCH_SEEDS"))
    parser.add_argument("--results-port", type=int, default=80)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args(argv)
    if not 1 <= args.results_port <= 65535:
        parser.error("--results-port must be between 1 and 65535")
    runtime.check_environment(os.environ)
    if not args.root or not args.seeds:
        raise RuntimeError("WORKBENCH_ROOT and WORKBENCH_SEEDS must be set in the image")
    root = Path(args.root)
    project = runtime.prepare_workspace(root, args.seeds)
    os.environ.update({"WORKBENCH_ROOT": str(root), "HOME": str(root / "home"),
                       "TMPDIR": str(root / "tmp"), "XDG_DATA_HOME": str(root / "data"),
                       "SUBSTRATE_SOURCE": str(root / "substrate"),
                       "GOCACHE": str(root / "cache/go-build"),
                       "GOMODCACHE": str(root / "cache/go-mod"), "GOPATH": str(root / "go")})
    runtime.write_status(root, "starting", "Starting the isolated Docker daemon")
    with (root / "logs/http.log").open("ab", buffering=0) as output:
        subprocess.Popen([sys.executable, "-m", "http.server", str(args.results_port), "--bind", "0.0.0.0",
                          "--directory", str(root / "results")],
                         stdout=output, stderr=subprocess.STDOUT)
    try:
        bootstrap.prepare(root, os.environ.get("container", ""))
        runtime.start_docker(root)
    except RuntimeError as error:
        runtime.write_status(root, "failed", str(error))
        print(f"Workbench startup failed: {error}", flush=True)
        os.execvp("sleep", ["sleep", "infinity"])
        return 1
    command = args.command
    if command[:1] == ["--"]:
        command = command[1:]
    if not command:
        command = ["bash", str(project / ".ci/workbench.sh")]
    os.chdir(project)
    runtime.write_status(root, "ready", "Workspace and Docker are ready")
    print(f"Workbench Docker is ready; results are served on port {args.results_port}", flush=True)
    os.execvp(command[0], command)
