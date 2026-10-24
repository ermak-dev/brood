"""Проба готовности `/healthz`: сервер отвечает и достаёт до своей базы.

Middleware, а не view: проба приходит раньше проверки Host, а kubelet
ходит по адресу пода, которого нет в ALLOWED_HOSTS.
"""
from django.db import DatabaseError, connection
from django.http import HttpResponse

PATH = "/healthz"


def healthz(get_response):
    def middleware(request):
        if request.path != PATH:
            return get_response(request)
        try:
            connection.ensure_connection()
        except DatabaseError as e:
            return HttpResponse(f"database unavailable: {e}\n", status=503,
                                content_type="text/plain")
        return HttpResponse("ok\n", content_type="text/plain")
    return middleware
