"""Сущности Substrate за REST: Atespace (проект), Actor (актор),
ActorTemplate (шаблон), Worker (воркер), AccessPolicy (политика доступа).

Всё -- прямое отражение RPC ateapi из api/ateapi/v1/ateapi.proto; состояния в
Django нет, источник истины -- ateapi. Удаление непустой atespace откажет на
стороне Substrate (FAILED_PRECONDITION): панель показывает причину. Политика
доступа -- одна на скоуп с именем default; update требует uid и version от
прочитанного (оптимистичная блокировка ateapi).
"""
from grpc import RpcError
from rest_framework import exceptions, permissions
from rest_framework.response import Response
from rest_framework.views import APIView

from brood.server.ateapi import ateapi_pb2
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
    "ABORTED": (409, None),
}

# Состояние актора для панели: короткое имя вместо ACTOR_STATE_RUNNING.
ACTOR_STATES = {value: name.removeprefix("ACTOR_STATE_").lower()
                for name, value in ateapi_pb2.ActorState.items()}

WORKER_STATES = {value: name.removeprefix("WORKER_STATE_").lower()
                 for name, value in ateapi_pb2.WorkerState.items()}

# Роли политик: глобальная -- owner/viewer, atespace -- еще и editor
# (cmd/ateapi/internal/apivalidation/access_policy.go).
GLOBAL_ROLES = ("owner", "viewer")
ATESPACE_ROLES = ("owner", "editor", "viewer")


def meta_json(metadata):
    """ResourceMetadata -> общая часть ответа: имя, uid, отметки времени."""
    return {"name": metadata.name, "uid": metadata.uid, "version": metadata.version,
            "create_time": metadata.create_time.ToJsonString().replace("Z", "+00:00"),
            "update_time": metadata.update_time.ToJsonString().replace("Z", "+00:00")}


def atespace_json(atespace):
    """Atespace -> словарь ответа."""
    return meta_json(atespace.metadata)


def actor_json(actor):
    """Actor -> словарь ответа: метаданные и состояние."""
    out = meta_json(actor.metadata)
    out["state"] = ACTOR_STATES.get(actor.status.state, "")
    return out


def template_json(template):
    """ActorTemplate -> словарь ответа: метаданные и число контейнеров."""
    out = meta_json(template.metadata)
    out["containers"] = len(template.containers)
    return out


def worker_json(worker):
    """Worker -> словарь ответа: метаданные, где живёт и чем занят."""
    out = meta_json(worker.metadata)
    status = worker.status
    out.update({"state": WORKER_STATES.get(status.state, ""),
                "worker_pool": worker.worker_pool,
                "node": worker.node_name,
                "actors_capacity": status.capacity.actors,
                "actors_allocated": status.allocated.actors})
    return out


def policy_json(policy):
    """AccessPolicy -> словарь ответа: uid/version (нужны update) и bindings."""
    out = meta_json(policy.metadata)
    out["bindings"] = [{"role": b.role, "members": list(b.members)}
                       for b in policy.bindings]
    return out


def policy_from(data, atespace=""):
    """Тело запроса -> AccessPolicy: имя фиксируем default, роли и members --
    из запроса, uid/version -- от прочитанного (их шлёт панель)."""
    bindings = []
    for b in data.get("bindings", []):
        role, members = str(b.get("role", "")), [str(m) for m in b.get("members", [])]
        bindings.append(ateapi_pb2.Binding(role=role, members=members))
    return ateapi_pb2.AccessPolicy(
        metadata=ateapi_pb2.ResourceMetadata(
            atespace=atespace, name=str(data.get("name", "default")),
            uid=str(data.get("uid", "")), version=int(data.get("version", 0))),
        bindings=bindings)


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


class ActorView(APIView):
    """/api/projects/<project>/actors[/<name>]: список акторов проекта и
    действия над одним; создание -- отдельный слой (шаблоны), сюда не входит."""

    permission_classes = [SessionSuperuser]

    def get(self, _request, project, name=None):
        if name:
            return Response(actor_json(call(client_of().get_actor, project, name)))
        return Response(
            {"results": [actor_json(a) for a in call(client_of().list_actors, project)]})

    def post(self, request, project, name):
        """Действие над актором: suspend или resume."""
        action = str(request.data.get("action", "")).strip().lower()
        rpcs = {"suspend": "suspend_actor", "resume": "resume_actor"}
        if action not in rpcs:
            return Response({"detail": "action must be suspend or resume"}, status=400)
        rpc = getattr(client_of(), rpcs[action])
        return Response(actor_json(call(rpc, project, name)))

    def delete(self, _request, project, name):
        call(client_of().delete_actor, project, name)
        return Response(status=204)


class TemplateView(APIView):
    """/api/projects/<project>/templates[/<name>]: шаблоны акторов проекта,
    только чтение и удаление -- создание ждёт своего слоя (контейнеры, тома)."""

    permission_classes = [SessionSuperuser]

    def get(self, _request, project, name=None):
        if name:
            return Response(template_json(call(client_of().get_actor_template, project, name)))
        return Response({"results": [template_json(t) for t in
                                     call(client_of().list_actor_templates, project)]})

    def delete(self, _request, project, name):
        call(client_of().delete_actor_template, project, name)
        return Response(status=204)


class WorkerView(APIView):
    """/api/workers[/<name>]: воркеры установки; drain -- действие над
    воркером (пометить завершающимся, планировщик больше не сажает)."""

    permission_classes = [SessionSuperuser]

    def get(self, _request, name=None):
        if name:
            return Response(worker_json(call(client_of().get_worker, name)))
        return Response({"results": [worker_json(w) for w in call(client_of().list_workers)]})

    def post(self, request, name):
        if str(request.data.get("action", "")).strip().lower() != "drain":
            return Response({"detail": "action must be drain"}, status=400)
        return Response(worker_json(call(client_of().drain_worker, name)))


class PolicyView(APIView):
    """/api/policy/global и /api/policy/projects/<project>: политика доступа
    скоупа. GET возвращает политику (404, если своей нет -- у atespace её и
    может не быть), PUT заменяет целиком: панель шлёт прочитанные uid и
    version, иначе ateapi откажет (оптимистичная блокировка)."""

    permission_classes = [SessionSuperuser]

    def get(self, _request, project=None):
        client = client_of()
        policy = (call(client.get_atespace_access_policy, project) if project
                  else call(client.get_global_access_policy))
        return Response(policy_json(policy))

    def put(self, request, project=None):
        client = client_of()
        policy = policy_from(request.data, atespace=project or "")
        roles = ATESPACE_ROLES if project else GLOBAL_ROLES
        wrong = [b["role"] for b in request.data.get("bindings", [])
                 if b.get("role") not in roles]
        if wrong:
            return Response({"detail": f"roles allowed here: {', '.join(roles)}"},
                            status=400)
        saved = (call(client.update_atespace_access_policy, project, policy) if project
                 else call(client.update_global_access_policy, policy))
        return Response(policy_json(saved))

    def delete(self, _request, project):
        call(client_of().delete_atespace_access_policy, project)
        return Response(status=204)
