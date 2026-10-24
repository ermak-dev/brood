"""the developer's own commands: the working copy's venv, the project's tracker and CI

  brood dev setup  create venv/ with the dependencies and the development settings in .env
  brood dev bug    project tracker: GitLab issues, one issue per unit of work
  brood dev ci     pipelines, job logs, lint and runners: GitLab CI from the terminal
  brood dev web    build and test the web panel
  brood dev proto  regenerate the ateapi gRPC stubs from the pinned Substrate

The tracker's coordinates come from the git origin of the brood working copy,
the credentials from its .env (GITLAB_TOKEN).
"""
from brood.dev.cli._common import usage


def main(argv):
    usage(__doc__)
