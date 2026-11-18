"""project merge requests: read or explicitly squash merge through GitLab

  brood dev mr show IID                 read a merge request without mutation
  brood dev mr merge IID [--sha SHA]    squash merge with CAS on current source HEAD

Merge reads HEAD itself. Optional --sha rejects a mismatch before writing.
No scheduling, source branch deletion, or automatic retry. After an unknown
outcome, inspect show in the same project before any retry.
Only state=merged is API-reported completion; Git/main is not verified.

Use --project group/project (or a numeric project ID) immediately after mr
or anywhere after the verb, including --project=group/project. It overrides
GITLAB_PROJECT, then git origin. GITLAB_SITE and intentionally configured
GITLAB_TOKEN use existing config: environment overrides the working copy's .env.
This grants no GitLab rights and does not bypass branch protection.
"""
from brood.dev.cli._common import usage


def main(argv):
    usage(__doc__)
