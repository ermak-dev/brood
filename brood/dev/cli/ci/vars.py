"""brood dev ci vars: the project's CI/CD variables with their type and flags, never their values"""
from brood.dev import gitlab
from brood.dev.cli.ci._common import parser


def line(var):
    """Строка переменной без значения. Чистая функция."""
    flags = [name for name in ("protected", "masked", "raw") if var.get(name)]
    return (f"{var['key']:<28} {var.get('variable_type', ''):<8} "
            f"{','.join(flags) or '-':<22} {var.get('environment_scope', '*')}")


def main(argv):
    parser("vars").parse_args(argv)
    found = sorted(gitlab.variables(), key=lambda var: var["key"])
    if not found:
        print("no CI/CD variables")
    for var in found:
        print(line(var))
    return 0
