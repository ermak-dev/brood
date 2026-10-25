"""Клиент ateapi приложения projects: один на процесс, пути -- из настроек.

Вынесен из views.py, чтобы проверки подменяли client_of, не трогая Django.
"""
import functools

from django.conf import settings

from brood.server.ateapi import client as ateapi

# Заменяется проверками на подделку; None -- «ещё не создан».
_testing = None


@functools.cache
def _client():
    return ateapi.Client(settings.ATEAPI_TOKEN, settings.ATEAPI_CA,
                         settings.ATEAPI_ENDPOINT or ateapi.ENDPOINT)


def client_of():
    """Клиент ateapi; в подах -- один на процесс. None в настройках -- нет
    projected-тома (локальная копия без кластера): проекты недоступны, панель
    получит 503 с причиной."""
    if _testing is not None:
        return _testing
    if not settings.ATEAPI_TOKEN or not settings.ATEAPI_CA:
        from brood.server.projects.views import ApiError
        raise ApiError(503, "ateapi is not configured: ATEAPI_TOKEN and ATEAPI_CA "
                            "point the server at its projected ServiceAccount token "
                            "and ClusterTrustBundle (see brood/dev/deploy/manifests/django.yaml)")
    return _client()
