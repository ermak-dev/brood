"""brood dev ci set VAR=VALUE ... [--file VAR=PATH ...]: create or update the project's CI/CD variables

VAR=VALUE sets a plain variable. --file VAR=PATH sets a file variable from
the contents of PATH: use it for secrets and anything multi-line, since a
value on the command line stays in the shell history. Every variable is
protected, so only pipelines of protected branches see it, and raw, so
GitLab does not expand $NAME inside it. Values are never printed.

  brood dev ci set --file BROOD_DEPLOY_ENV=~/etc/secrets/brood/stand.env
"""
import os

from brood.dev import gitlab
from brood.dev.cli.ci._common import parser


def split(pair, what):
    """VAR=VALUE -> (VAR, VALUE). Чистая функция."""
    key, sep, value = pair.partition("=")
    if not sep or not key:
        raise RuntimeError(f"brood dev ci set: {what} must be VAR=..., got {pair!r}")
    return key, value


def read(path):
    path = os.path.expanduser(path)
    try:
        with open(path, encoding="utf-8") as f:
            text = f.read()
    except OSError as e:
        raise RuntimeError(f"cannot read {path}: {e.strerror}") from None
    if not text.strip():
        raise RuntimeError(f"{path} is empty")
    return text


def main(argv):
    p = parser("set")
    p.add_argument("pairs", nargs="*")
    p.add_argument("--file", action="append", default=[])
    a = p.parse_args(argv)
    # Всё разобрано и прочитано до первого запроса: опечатка во втором
    # аргументе не оставляет первый уже записанным.
    todo = [(*split(pair, "an argument"), False) for pair in a.pairs]
    for pair in a.file:
        key, path = split(pair, "--file")
        todo.append((key, read(path), True))
    if not todo:
        raise RuntimeError("brood dev ci set: name at least one VAR=VALUE or --file VAR=PATH")
    for key, value, is_file in todo:
        done = gitlab.set_variable(key, value, file=is_file)
        print(f"{key} ({'file' if is_file else 'variable'}, protected, raw): {done}")
    return 0
