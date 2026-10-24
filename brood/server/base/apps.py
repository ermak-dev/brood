from django.apps import AppConfig


class BaseConfig(AppConfig):
    name = "brood.server.base"
    default_auto_field = "django.db.models.BigAutoField"
