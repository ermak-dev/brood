"""manage.py сервера: `brood server <команда>` или `python -m brood.server.manage`."""
import os
import sys


def main(argv):
    os.environ.setdefault("DJANGO_SETTINGS_MODULE", "brood.server.settings")
    from django.core.management import execute_from_command_line
    execute_from_command_line(["brood server", *argv])


if __name__ == "__main__":
    main(sys.argv[1:])
