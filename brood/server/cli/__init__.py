"""the installation's server: a Django project, and `brood server` is its manage.py

  brood server                        Django's commands
  brood server migrate                apply migrations to the database
  brood server createsuperuser        an account for the admin at /admin/
  brood server runserver [addr:port]  the development server
  brood server test [label ...]       every check of brood, or the named ones

Settings come from the working copy's .env: DJANGO_SECRET_KEY and
DJANGO_DEBUG (1 or 0). `brood dev setup` creates the venv with Django and
adds both to .env.
"""
from brood.server import manage


def main(argv):
    try:
        from django.core.exceptions import ImproperlyConfigured
    except ImportError:
        raise ImportError("Django is not installed: run brood dev setup") from None
    try:
        manage.main(argv)
    except ImproperlyConfigured as e:
        # Отказ настроек -- строкой, как любой отказ командлета: Django
        # отдал бы его трассировкой.
        raise RuntimeError(str(e)) from None
