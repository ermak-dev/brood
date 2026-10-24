"""WSGI-вход сервера: application для runserver и сервера приложений."""
import os

from django.core.wsgi import get_wsgi_application

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "brood.server.settings")
application = get_wsgi_application()
