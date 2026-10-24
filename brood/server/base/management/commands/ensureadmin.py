"""brood server ensureadmin: завести суперпользователя из настроек, если его нет.

Учётка -- из DJANGO_SUPERUSER_USERNAME, DJANGO_SUPERUSER_PASSWORD и
необязательного DJANGO_SUPERUSER_EMAIL. Выкладка зовёт команду каждый раз
(init-контейнер), поэтому существующий пользователь остаётся как есть:
пароль, сменённый в админке, выкладка не сбрасывает.
"""
from django.contrib.auth import get_user_model
from django.core.management.base import BaseCommand, CommandError

from brood.common import config


class Command(BaseCommand):
    help = "Create the superuser DJANGO_SUPERUSER_USERNAME unless it exists; keep an existing one."

    def handle(self, **options):
        username = config.get("DJANGO_SUPERUSER_USERNAME")
        if not username:
            self.stdout.write("DJANGO_SUPERUSER_USERNAME is not set: no superuser to ensure")
            return
        users = get_user_model().objects
        if users.filter(username=username).exists():
            self.stdout.write(f"superuser {username} exists: kept as it is")
            return
        password = config.get("DJANGO_SUPERUSER_PASSWORD")
        if not password:
            raise CommandError(f"DJANGO_SUPERUSER_PASSWORD is not set: cannot create {username}")
        users.create_superuser(username, config.get("DJANGO_SUPERUSER_EMAIL"), password)
        self.stdout.write(f"superuser {username} created")
