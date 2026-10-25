"""Сущности Substrate за REST: Atespace (проект), Actor (актор),
ActorTemplate (шаблон), Worker (воркер), AccessPolicy (политика доступа).

Тонкий прокси к RPC ateapi (api/ateapi/v1/ateapi.proto): состояния в Django
нет, источник истины -- ateapi, он же валидирует. Объекты ходят в
каноническом protojson ateapi -- том же, что печатает `kubectl ate get -o
yaml` и что лежит в демо-шаблонах Substrate; так панель видит и сохраняет и
те поля, которых её формы не знают. Проект в пути -- скоуп: atespace из тела
не принимается, его ставит сервер.
"""
from google.protobuf import json_format
from grpc import RpcError
from rest_framework import exceptions, permissions
from rest_framework.response import Response
from rest_framework.views import APIView

from brood.server.ateapi import ateapi_pb2
from brood.server.ateapi.client import POLICY
from brood.server.projects.client_of import client_of

# grpc.StatusCode -> HTTP. Причина -- всегда message ateapi: у одного кода
# разные поводы (FAILED_PRECONDITION -- и «atespace не пуст», и «atespace нет»).
FOR_HTTP = {
    "NOT_FOUND": 404,
    "ALREADY_EXISTS": 409,
    "ABORTED": 409,
    "FAILED_PRECONDITION": 409,
    "INVALID_ARGUMENT": 400,
    "PERMISSION_DENIED": 403,
    "UNAUTHENTICATED": 403,
    "UNAVAILABLE": 503,
}


class SessionSuperuser(permissions.BasePermission):
    """Сессия Django суперпользователя: как у /v8/management (nginx спросит
    /auth/llm, Django -- этот класс). CSRF проверяет SessionAuthentication
    DRF по куке csrftoken и заголовку X-CSRFToken панели."""

    def has_permission(self, request, _view):
        return bool(request.user and request.user.is_authenticated and request.user.is_superuser)


class ApiError(exceptions.APIException):
    """Отказ, готовый к ответу: статус HTTP и причина строкой (MUST: отказ
    несёт причину, не трассировку)."""

    def __init__(self, status, detail):
        super().__init__(detail)
        self.status_code = status


def call(rpc, *args):
    """rpc(*args) -> ответ; отказ grpc -> ApiError с причиной ateapi."""
    try:
        return rpc(*args)
    except RpcError as e:
        raise ApiError(FOR_HTTP.get(e.code().name, 502),
                       e.details() or e.code().name) from None


def as_json(message):
    return json_format.MessageToDict(message)


def results(messages):
    return Response({"results": [as_json(m) for m in messages]})


def parse(message_class, data):
    """Тело запроса (protojson) -> сообщение ateapi; неизвестное поле или
    неверный тип -- 400 с причиной, а не 500."""
    if not isinstance(data, dict):
        raise ApiError(400, f"the body must be a JSON object ({message_class.DESCRIPTOR.name})")
    try:
        return json_format.ParseDict(data, message_class())
    except (json_format.ParseError, TypeError, ValueError) as e:
        raise ApiError(400, f"invalid {message_class.DESCRIPTOR.name}: {e}") from None


def only(request, present):
    """Метод не для этой формы пути (коллекция или элемент): Django отдаёт
    вью путь без несовпавшей группы, и без проверки вышел бы TypeError 500."""
    if not present:
        raise exceptions.MethodNotAllowed(request.method)


def action(request, allowed):
    """Действие над ресурсом из тела {"action": ...}; другое -- 400."""
    data = request.data if isinstance(request.data, dict) else {}
    name = str(data.get("action", "")).strip().lower()
    if name not in allowed:
        raise ApiError(400, f"action must be {' or '.join(allowed)}")
    return name


class ProjectView(APIView):
    """/api/projects[/<name>]: atespace установки. Изменяемых полей у
    atespace нет -- только создание и удаление (непустую ateapi не удалит)."""

    permission_classes = [SessionSuperuser]

    def get(self, _request, name=None):
        if name:
            return Response(as_json(call(client_of().get_atespace, name)))
        return results(call(client_of().list_atespaces))

    def post(self, request, name=None):
        only(request, name is None)
        atespace = parse(ateapi_pb2.Atespace, request.data)
        return Response(as_json(call(client_of().create_atespace, atespace)), status=201)

    def delete(self, request, name=None):
        only(request, name)
        call(client_of().delete_atespace, name)
        return Response(status=204)


class ActorView(APIView):
    """/api/projects/<project>/actors[/<name>]: акторы проекта. PUT меняет
    шаблон и workerSelector (uid и version прочитанного -- предусловие), POST
    на актора -- suspend или resume."""

    permission_classes = [SessionSuperuser]

    def get(self, _request, project, name=None):
        if name:
            return Response(as_json(call(client_of().get_actor, project, name)))
        return results(call(client_of().list_actors, project))

    def post(self, request, project, name=None):
        if name:
            verb = action(request, ("suspend", "resume"))
            return Response(as_json(call(getattr(client_of(), f"{verb}_actor"), project, name)))
        actor = parse(ateapi_pb2.Actor, request.data)
        actor.metadata.atespace = project
        return Response(as_json(call(client_of().create_actor, actor)), status=201)

    def put(self, request, project, name=None):
        only(request, name)
        actor = parse(ateapi_pb2.Actor, request.data)
        actor.metadata.atespace, actor.metadata.name = project, name
        return Response(as_json(call(client_of().update_actor, actor)))

    def delete(self, request, project, name=None):
        only(request, name)
        call(client_of().delete_actor, project, name)
        return Response(status=204)


class TemplateView(APIView):
    """/api/projects/<project>/templates[/<name>]: шаблоны акторов проекта.
    Шаблон неизменяем (Update в ateapi нет): правка -- новый шаблон."""

    permission_classes = [SessionSuperuser]

    def get(self, _request, project, name=None):
        if name:
            return Response(as_json(call(client_of().get_actor_template, project, name)))
        return results(call(client_of().list_actor_templates, project))

    def post(self, request, project, name=None):
        only(request, name is None)
        template = parse(ateapi_pb2.ActorTemplate, request.data)
        template.metadata.atespace = project
        return Response(as_json(call(client_of().create_actor_template, template)), status=201)

    def delete(self, request, project, name=None):
        only(request, name)
        call(client_of().delete_actor_template, project, name)
        return Response(status=204)


class WorkerView(APIView):
    """/api/workers[/<name>]: воркеры установки -- зеркала подов, их пишет
    ate-controller (метки -- из WorkerPool, доступность -- по готовности
    пода). Руками долговечно только drain: он финальный."""

    permission_classes = [SessionSuperuser]

    def get(self, _request, name=None):
        if name:
            return Response(as_json(call(client_of().get_worker, name)))
        return results(call(client_of().list_workers))

    def post(self, request, name=None):
        only(request, name)
        action(request, ("drain",))
        return Response(as_json(call(client_of().drain_worker, name)))


class PolicyView(APIView):
    """/api/policy/global и /api/policy/projects/<project>: политика доступа
    скоупа. POST заводит, PUT заменяет целиком (uid и version прочитанного --
    предусловие), DELETE -- только у проекта: глобальную ateapi не удаляет.
    Имя -- всегда default, atespace в metadata ateapi запрещает."""

    permission_classes = [SessionSuperuser]

    def get(self, _request, project=None):
        client = client_of()
        if project:
            return Response(as_json(call(client.get_atespace_access_policy, project)))
        return Response(as_json(call(client.get_global_access_policy)))

    def post(self, request, project=None):
        client, policy = client_of(), self._policy(request)
        if project:
            saved = call(client.create_atespace_access_policy, project, policy)
        else:
            saved = call(client.create_global_access_policy, policy)
        return Response(as_json(saved), status=201)

    def put(self, request, project=None):
        client, policy = client_of(), self._policy(request)
        if project:
            return Response(as_json(call(client.update_atespace_access_policy, project, policy)))
        return Response(as_json(call(client.update_global_access_policy, policy)))

    def delete(self, _request, project=None):
        if not project:
            raise ApiError(405, "the global access policy cannot be deleted; "
                                "save it with no bindings instead")
        call(client_of().delete_atespace_access_policy, project)
        return Response(status=204)

    def _policy(self, request):
        policy = parse(ateapi_pb2.AccessPolicy, request.data)
        policy.metadata.atespace, policy.metadata.name = "", POLICY
        return policy
