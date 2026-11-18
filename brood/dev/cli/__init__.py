"""the developer's own commands: the working copy's venv, the project's tracker and CI

  brood dev setup  create venv/ with the dependencies and the development settings in .env
  brood dev bug    project tracker: GitLab issues, one issue per unit of work
  brood dev ci     pipelines, job logs, lint and runners: GitLab CI from the terminal
  brood dev mr     project merge requests: read or explicitly squash merge through GitLab
  brood dev test   run Python unittest tests without a server runtime

The tracker's coordinates come from the git origin of the brood working copy,
the credentials from its .env (GITLAB_TOKEN).
"""
from brood.dev import gitlab
from brood.dev.cli._common import usage


def dispatch(argv, run):
    """Общий параметр проекта обрабатывается до выбора командлета."""
    if len(argv) < 2 or argv[1] not in ("bug", "ci", "mr"):
        return run(argv)
    result = argv[:2]
    selected = None
    rest = argv[2:]
    while rest:
        arg, *rest = rest
        if arg == "--":
            result.extend([arg, *rest])
            break
        if arg == "--project" or arg.startswith("--project="):
            if selected is not None:
                raise RuntimeError("--project may be specified only once")
            if arg == "--project":
                if not rest or rest[0].startswith("-"):
                    raise RuntimeError("--project requires a group/project path")
                selected, *rest = rest
            else:
                selected = arg.partition("=")[2]
        else:
            result.append(arg)
    if selected is None:
        return run(result)
    with gitlab.project_scope(selected):
        return run(result)



def main(argv):
    usage(__doc__)
