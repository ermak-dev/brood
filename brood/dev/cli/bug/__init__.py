"""project tracker: GitLab issues, one issue per unit of work

  brood dev bug list [--all] [--label L] [-t TEXT]   open issues, worst first
  brood dev bug show <iid>                           full body
  brood dev bug new "<title>" [--label L ...]        body from --body, --body-file or stdin
  brood dev bug new "<title>" --epic 12              child of epic #12
  brood dev bug evidence <iid>                       fetched branches and integration merges
  brood dev bug epic <iid>                           the epic's children and progress
  brood dev bug edit <iid> [--title T] [--body TEXT|--body-file F]  replace the title and/or body
  brood dev bug comment <iid> "<text>"               comment (or - for stdin)
  brood dev bug start <iid> [slug] [--no-branch]     claim: status::wip + branch
  brood dev bug close <iid> --comment "..."          close, default status::fixed
  brood dev bug relabel <iid> [--status S]           change labels; live, wip, parked reopen
  brood dev bug labels                               the label vocabulary

Select a project with --project group/project, immediately after `bug` or
anywhere after the verb (also --project=group/project). It overrides
GITLAB_PROJECT in .env, then the working copy's git origin. GITLAB_SITE and
GITLAB_TOKEN still come from .env; --project never changes the GitLab site.
`start` with a branch and `evidence` verify the selected site and project
against the checkout's origin. For an explicit project different from the
default, run inside that project's worktree; otherwise the brood checkout is
used from any directory. `start --no-branch` only updates the tracker.
Titles, bodies and comments are in Russian; branch names and commits stay English.
"""
from brood.dev.cli._common import usage


def main(argv):
    usage(__doc__)
