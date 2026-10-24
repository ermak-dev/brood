"""brood server serve --bind ADDR: сервер под gunicorn, WSGI-сервером продакшена.

Команда, а не вызов gunicorn: и в контейнере всё идёт через bin/brood.
Адрес -- аргумент без дефолта: в поде это 127.0.0.1:8000 за nginx
(brood/dev/deploy/manifests/server.yaml).
"""
from django.core.management.base import BaseCommand
from gunicorn.app.base import BaseApplication


class Gunicorn(BaseApplication):
    """gunicorn с настройками словарём, без разбора своего argv."""

    def __init__(self, app, options):
        self.app, self.options = app, options
        super().__init__()

    def load_config(self):
        for name, value in self.options.items():
            self.cfg.set(name, value)

    def load(self):
        return self.app


class Command(BaseCommand):
    help = "Serve brood under gunicorn: --bind ADDR, --workers N (2 by default)."

    def add_arguments(self, parser):
        parser.add_argument("--bind", required=True)
        parser.add_argument("--workers", type=int, default=2)

    def handle(self, **options):
        from brood.server.wsgi import application
        Gunicorn(application, {"bind": options["bind"], "workers": options["workers"],
                               "accesslog": "-", "errorlog": "-"}).run()
