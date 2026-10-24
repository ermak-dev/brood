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

Coordinates come from the working copy's git origin; credentials are
GITLAB_TOKEN, a personal access token, in .env. Titles, bodies
and comments are in Russian; branch names and commits stay English.
"""
from brood.dev.cli._common import usage


def main(argv):
    usage(__doc__)
