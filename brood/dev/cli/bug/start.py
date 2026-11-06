"""brood dev bug start <iid> [slug] [--no-branch] [--worker W]: claim an issue

Sets status::wip and, unless --no-branch, creates the issue's branch from
the integration branch.
"""
import subprocess

from brood.dev import gitlab
from brood.dev.cli._common import default_branch, git
from brood.dev.cli.bug._common import parser


def main(argv):
    """Взять задачу: метка wip плюс ветка.

    Ветку заводит исполнитель в своём клоне, поэтому у мастера есть
    --no-branch: общий чекаут мастера не переключается никогда."""
    p = parser("start")
    p.add_argument("iid")
    p.add_argument("slug", nargs="?")
    p.add_argument("--worker")
    p.add_argument("--no-branch", action="store_true")
    a = p.parse_args(argv)
    if not a.no_branch:
        checkout = gitlab.require_local_project()
    i = gitlab.issue(a.iid)
    branch = gitlab.branch_name(i["labels"], a.iid, a.slug or i["title"])
    if not a.no_branch:
        git("fetch", "origin", cwd=checkout)
        base = default_branch(cwd=checkout)
        git("rev-parse", "--verify", base + "^{commit}", cwd=checkout)
        git("check-ref-format", "--branch", branch, cwd=checkout)
        exists = subprocess.run(["git", "show-ref", "--verify", "--quiet", "refs/heads/" + branch],
                                cwd=checkout, capture_output=True)
        if exists.returncode == 0:
            raise RuntimeError(f"branch already exists: {branch}")
        if exists.returncode != 1:
            raise RuntimeError("cannot check whether branch already exists")
    gitlab.relabel(a.iid, gitlab.with_status(i["labels"], "wip"))
    note = f"Работа начата в ветке `{branch}`"
    if a.worker:
        note += f" · воркер {a.worker}"
    gitlab.comment(a.iid, note + ".")
    if a.no_branch:
        print(f"#{a.iid} claimed; branch for the executor: {branch}")
        return
    git("checkout", "-b", branch, base, cwd=checkout)
    print(f"#{a.iid} claimed, on {branch}")
