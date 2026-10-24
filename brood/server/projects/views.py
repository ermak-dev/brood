"""Проекты brood: Atespace Substrate за REST.

Atespace -- пространство имён акторов установки (docs/SUBSTRATE.md), в
панели brood это «проект». CRUD здесь -- прямое отражение RPC CreateAtespace,
GetAtespace, ListAtespaces, DeleteAtespace из api/ateapi/v1/ateapi.proto;
состояния в Django нет, источник истины -- ateapi. Удаление непустой atespace
откажет на стороне Substrate (FAILED_PRECONDITION): панель показывает причину.
"""
from grpc import RpcError
from rest_framework import exceptions, permissions
from rest_framework.response import Response
from rest_framework.views import APIView

from brood.server.projects.client_of import client_of

# grpc.StatusCode -> (HTTP, причина). None -- причина в message самого grpc.
FAILED_PRECONDITION = "atespace has actors or tags left; empty it first"
FOR_HTTP = {
    "NOT_FOUND": (404, None),
    "ALREADY_EXISTS": (409, None),
    "FAILED_PRECONDITION": (409, FAILED_PRECONDITION),
    "INVALID_ARGUMENT": (400, None),
    "PERMISSION_DENIED": (403, None),
    "UNAUTHENTICATED": (403, None),
    "UNAVAILABLE": (503, None),
}


def atespace_json(atespace):
    """Atespace -> словарь ответа: поля ResourceMetadata, отметки времени --
    ISO 8601 с зоной."""
    meta = atespace.metadata
    return {"name": meta.name, "uid": meta.uid, "version": meta.version,
            "create_time": meta.create_time.ToJsonString().replace("Z", "+00:00"),
            "update_time": meta.update_time.ToJsonString().replace("Z", "+00:00")}


class SessionSuperuser(permissions.BasePermission):
    """Сессия Django суперпользователя: как у /v8/management (nginx спросит
    /auth/llm, Django -- этот класс). CSRF проверяет SessionAuthentication
    DRF по куке csrftoken и заголовку X-CSRFToken панели."""

    def has_permission(self, request, _view):
        return bool(request.user and request.user.is_authenticated and request.user.is_superuser)


class ApiError(exceptions.APIException):
    """Отказ ateapi, готовый к ответу: статус HTTP и причина строкой (MUST:
    отказ несёт причину, не трассировку)."""

    def __init__(self, status, detail):
        super().__init__(detail)
        self.status_code = status


def call(rpc, *args):
    """rpc(*args) -> ответ; отказ grpc -> ApiError."""
    try:
        return rpc(*args)
    except RpcError as e:
        status, detail = FOR_HTTP.get(e.code().name, (502, f"ateapi: {e.details()}"))
        raise ApiError(status, detail or e.details() or e.code().name) from None


class ProjectView(APIView):
    """/api/projects и /api/projects/<name>: панель читает список целиком,
    пагинация ateapi (page_token, потолок 1000) раскрывается в клиенте."""

    permission_classes = [SessionSuperuser]

    def get(self, _request, name=None):
        if name:
            return Response(atespace_json(call(client_of().get_atespace, name)))
        return Response(
            {"results": [atespace_json(a) for a in call(client_of().list_atespaces)]})

    def post(self, request):
        name = str(request.data.get("name", "")).strip()
        if not name:
            return Response({"detail": "name is required"}, status=400)
        return Response(atespace_json(call(client_of().create_atespace, name)), status=201)

    def delete(self, _request, name):
        call(client_of().delete_atespace, name)
        return Response(status=204)
