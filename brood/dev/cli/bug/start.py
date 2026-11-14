"""brood dev bug start <iid> [slug] [--no-branch] [--worker W]: claim an issue

Unless --no-branch, creates the issue's branch from the integration branch
before setting status::wip and posting the issue note. The note and output name
the branch by the CLAUDE.md rule: the type label word, the iid and the slug
(the ASCII words of the title unless given). With --no-branch the executor
creates exactly that branch. Tracker failures preserve the local branch and
report the confirmed state and tracker-only recovery commands.
"""
import shlex
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
    project = shlex.quote(gitlab.project())
    recovery = (f"Check 'brood dev bug show {a.iid} --project {project}' in the same project; "
                f"if needed, set wip with 'brood dev bug relabel {a.iid} --status wip --project {project}' "
                f"and add the missing branch note with 'brood dev bug comment {a.iid} --body-file - --project {project}'. "
                "Check existing notes at the issue URL before posting again.")
    if not a.no_branch:
        git("fetch", "origin", cwd=checkout)
        base = default_branch(cwd=checkout)
        git("rev-parse", "--verify", base + "^{commit}", cwd=checkout)
        git("check-ref-format", "--branch", branch, cwd=checkout)
        exists = subprocess.run(["git", "show-ref", "--verify", "--quiet", "refs/heads/" + branch],
                                cwd=checkout, capture_output=True)
        if exists.returncode == 0:
            raise RuntimeError(f"branch already exists: {branch}; no tracker changes made. {recovery}")
        if exists.returncode != 1:
            raise RuntimeError("cannot check whether branch already exists")
        # Checkout может отказать из-за локальных правок: трекер ещё не трогаем.
        # При отказе API ветку сохраняем: откат мог бы уничтожить работу, а
        # потерянный ответ не доказывает, что сервер не применил запрос (#301).
        git("checkout", "-b", branch, base, cwd=checkout)
    # С --no-branch ветку заводит исполнитель, ровно под этим именем (CLAUDE.md,
    # «Ветка задачи»): заметка говорит, что её может ещё не быть на origin.
    where = f"; ветку `{branch}` заведёт исполнитель" if a.no_branch else f" в ветке `{branch}`"
    note = f"Работа начата{where}"
    if a.worker:
        note += f" · воркер {a.worker}"
    claimed = False
    try:
        gitlab.relabel(a.iid, gitlab.with_status(i["labels"], "wip"))
        claimed = True
        gitlab.comment(a.iid, note + ".")
    except (RuntimeError, OSError) as e:
        state = "status::wip set; note not confirmed" if claimed else "claim not confirmed"
        local = f"branch for the executor: {branch}" if a.no_branch else f"checkout is on {branch}"
        raise RuntimeError(f"#{a.iid}: {state}; {local}; {e}. {recovery}") from None
    if a.no_branch:
        print(f"#{a.iid} claimed; branch for the executor: {branch}")
        return
    print(f"#{a.iid} claimed, on {branch}")
