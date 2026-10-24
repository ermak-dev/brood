"""brood dev ci lint [file]: validate a .gitlab-ci.yml the way GitLab would

The file is the working copy's .gitlab-ci.yml by default; its includes are
resolved. Prints "valid, N job(s): ..." or "invalid" with the errors
(exit 1), then warnings.
"""
import os

from brood.common import config
from brood.dev import gitlab
from brood.dev.cli.ci._common import parser


def main(argv):
    p = parser("lint")
    p.add_argument("file", nargs="?", default=os.path.join(config.PROJECT, ".gitlab-ci.yml"))
    a = p.parse_args(argv)
    try:
        with open(a.file, encoding="utf-8") as f:
            content = f.read()
    except OSError as e:
        raise RuntimeError(f"cannot read {a.file}: {e.strerror}") from None
    lines, valid = gitlab.lint_report(gitlab.lint(content))
    print("\n".join(lines))
    return 0 if valid else 1
