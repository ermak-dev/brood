"""brood dev ci log <job> [--lines N] [--full]: a job's log

The last 60 lines by default, terminal colors stripped.
"""
from brood.dev import gitlab
from brood.dev.cli._common import plain
from brood.dev.cli.ci._common import parser

LINES = 60


def main(argv):
    p = parser("log")
    p.add_argument("job", type=int)
    p.add_argument("--lines", type=int, default=LINES)
    p.add_argument("--full", action="store_true")
    a = p.parse_args(argv)
    text = gitlab.tail(plain(gitlab.trace(a.job)), None if a.full else a.lines)
    print(text or "(empty log)")
    return 0
