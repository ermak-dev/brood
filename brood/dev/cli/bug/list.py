"""brood dev bug list [--all] [--label L] [-t TEXT]: open issues, worst first
"""
from brood.common.render import table
from brood.dev import gitlab
from brood.dev.cli.bug._common import parser


def main(argv):
    p = parser("list")
    p.add_argument("--all", action="store_true")
    p.add_argument("--label", action="append", default=[])
    p.add_argument("-t", "--text")
    a = p.parse_args(argv)
    issues = gitlab.issues(state="all" if a.all else "opened",
                           labels=[gitlab.check_label(label) for label in a.label],
                           search=a.text)
    if not issues:
        print("no issues")
        return
    print("\n".join(table(
        [(f"#{i['iid']}",
          gitlab.scoped(i["labels"], "status::").removeprefix("status::") or "-",
          gitlab.scoped(i["labels"], "sev::").removeprefix("sev::") or "-",
          gitlab.scoped(i["labels"], "component::").removeprefix("component::") or "-",
          i["title"]) for i in issues])))
