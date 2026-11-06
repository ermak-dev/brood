"""pipelines, job logs, lint and runners: GitLab CI from the terminal

  brood dev ci                           the latest 15 pipelines
  brood dev ci <id> [--failed]           a pipeline's jobs; --failed: only real failures
  brood dev ci show <id> [--failed]      the same
  brood dev ci why <id>                  why it failed: failing jobs with their log tails
  brood dev ci log <job> [--lines N] [--full]
                                         a job's log, the last 60 lines by default
  brood dev ci lint [file]               validate a .gitlab-ci.yml, the working copy's by default
  brood dev ci retry <job>               run a finished job again
  brood dev ci retry --pipeline <id>     rerun a pipeline's failed jobs
  brood dev ci cancel <id>               cancel a pipeline that is still going
  brood dev ci runners                   the project's runners and what each is running
  brood dev ci set VAR=VALUE ... [--file VAR=PATH ...]
                                         create or update protected CI/CD variables
  brood dev ci vars                      CI/CD variables with type and flags, without values

Select a project with --project group/project, immediately after `ci` or
anywhere after the verb (also --project=group/project). It overrides
GITLAB_PROJECT in .env, then the working copy's git origin. GITLAB_SITE and
GITLAB_TOKEN still come from .env; --project never changes the GitLab site.
Lint reads the local working copy by default; pass a file for another project.
"""
import importlib

from brood.dev.cli._common import usage


def route(argv):
    """argv без глагола -> (глагол, argv) либо None. Пусто -- список, число --
    пайплайн: самое частое -- самое короткое. Глагол со своим модулем сюда
    не доходит: его отдаёт диспетчер."""
    if not argv:
        return "list", []
    if argv[0].isdigit():
        return "show", [*argv]
    return None


def main(argv):
    found = route(argv)
    if found is None:
        usage(__doc__)
    verb, rest = found
    return importlib.import_module(f"brood.dev.cli.ci.{verb}").main(rest)
