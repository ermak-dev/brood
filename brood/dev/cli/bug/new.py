"""brood dev bug new "<title>" [--label L ...] [--body TEXT|--body-file F] [--epic N]: open an issue

Body from --body, --body-file or stdin; --epic makes it a child of that epic. A
status label is added when none is given: status::live.
"""
from brood.dev import gitlab
from brood.dev.cli._common import usage
from brood.dev.cli.bug._common import parser, read_body


def main(argv):
    p = parser("new")
    p.add_argument("title")
    p.add_argument("--label", action="append", default=[])
    body = p.add_mutually_exclusive_group()
    body.add_argument("--body")
    body.add_argument("--body-file")
    p.add_argument("--epic")
    a = p.parse_args(argv)
    labels = [gitlab.check_label(label) for label in a.label]
    if not gitlab.scoped(labels, "status::"):
        labels.append("status::live")
    body = a.body.strip() if a.body is not None else read_body(None, a.body_file)
    if not body:
        usage(__doc__ + "\nissue body must not be empty")
    if a.epic:
        body = gitlab.with_epic(body, a.epic)
    i = gitlab.create(a.title, body, labels)
    print(f"#{i['iid']} {i['web_url']}")
