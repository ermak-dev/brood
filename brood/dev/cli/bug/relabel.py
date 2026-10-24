"""brood dev bug relabel <iid> [--status S] [--label L ...]: change labels

--status live, wip or parked also reopens a closed issue; fixed, noise and
dup are closing statuses, but only `brood dev bug close` closes.
"""
from brood.dev import gitlab
from brood.dev.cli.bug._common import parser


def main(argv):
    p = parser("relabel")
    p.add_argument("iid")
    p.add_argument("--status", choices=gitlab.OPEN_STATUSES + gitlab.CLOSE_STATUSES)
    p.add_argument("--label", action="append", default=[])
    a = p.parse_args(argv)
    i = gitlab.issue(a.iid)
    labels = gitlab.with_status(i["labels"], a.status) if a.status else list(i["labels"])
    for label in a.label:
        group = gitlab.check_label(label).split("::")[0]
        labels = [x for x in labels if not x.startswith(f"{group}::")] + [label]
    (gitlab.reopen if gitlab.relabel_action(i["state"], a.status) == "reopen"
     else gitlab.relabel)(a.iid, labels)
    print(f"#{a.iid}: {', '.join(labels)}")
