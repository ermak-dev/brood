"""gRPC-клиент ateapi сервера установки.

Стабы сгенерированы из api/ateapi/v1/ateapi.proto (brood dev proto).
Аутентификация -- projected ServiceAccount токен с audience api.ate-system.svc,
TLS -- ClusterTrustBundle signer'а servicedns.podcert.ate.dev/identity, как у
internal/ateclient/builder.go Substrate. Токен ротируется kubelet'ом и
перечитывается с диска перед каждым вызовом.
"""
import grpc

from brood.server.ateapi import ateapi_pb2, ateapi_pb2_grpc

# Адрес и audience api.ate-system.svc: ateapi проверяет ровно его.
AUDIENCE = "api.ate-system.svc"
ENDPOINT = f"{AUDIENCE}:443"
# Политика доступа -- синглтон скоупа, другого имени ateapi не примет.
POLICY = "default"


class BearerToken(grpc.AuthMetadataPlugin):
    """Токен из файла -- в каждый вызов: kubelet ротирует его подточку,
    канал пересобирать не нужно. Оборачивается metadata_call_credentials:
    composite_channel_credentials принимает только CallCredentials, голый
    класс с GetRequestMetadata падает AttributeError'ом."""

    def __init__(self, path):
        super().__init__()
        self.path = path

    def __call__(self, _context, callback):
        """gRPC зовет это при каждом запросе, в отдельном потоке; metadata --
        асинхронно, через callback, но чтение маленького файла здесь
        синхронно и мгновенно."""
        with open(self.path, encoding="utf-8") as f:
            callback((("authorization", f"Bearer {f.read().strip()}"),), None)


class Client:
    """Control-клиент ateapi. Пути до токена и доверенного корня передаются,
    чтобы подменять их в проверках."""

    def __init__(self, token_file, ca_file, endpoint=ENDPOINT):
        # Другой адрес -- port-forward разработчика, как у kubectl ate: TLS
        # всё равно сверяет имя сервиса, на которое выписан сертификат ateapi.
        options = [] if endpoint == ENDPOINT else [("grpc.ssl_target_name_override", AUDIENCE)]
        self.channel = grpc.secure_channel(
            endpoint,
            grpc.composite_channel_credentials(
                _server_credentials(ca_file),
                grpc.metadata_call_credentials(BearerToken(token_file))),
            options=options)

    def close(self):
        self.channel.close()

    # ─── Atespace ────────────────────────────────────────────────────────
    def list_atespaces(self):
        return self._list("ListAtespaces", "atespaces", ateapi_pb2.ListAtespacesRequest())

    def get_atespace(self, name):
        return self._stub().GetAtespace(ateapi_pb2.GetAtespaceRequest(atespace=_ref(name)))

    def create_atespace(self, atespace):
        return self._stub().CreateAtespace(ateapi_pb2.CreateAtespaceRequest(atespace=atespace))

    def delete_atespace(self, name):
        return self._stub().DeleteAtespace(ateapi_pb2.DeleteAtespaceRequest(atespace=_ref(name)))

    # ─── Actor ───────────────────────────────────────────────────────────
    def list_actors(self, atespace):
        return self._list("ListActors", "actors",
                          ateapi_pb2.ListActorsRequest(atespace=atespace))

    def get_actor(self, atespace, name):
        return self._stub().GetActor(ateapi_pb2.GetActorRequest(actor=_ref(name, atespace)))

    def create_actor(self, actor):
        return self._stub().CreateActor(ateapi_pb2.CreateActorRequest(actor=actor))

    def update_actor(self, actor):
        """Замена изменяемых полей (шаблон, workerSelector); uid и version в
        metadata -- предусловие: ateapi откажет ABORTED, если актор менялся."""
        return self._stub().UpdateActor(ateapi_pb2.UpdateActorRequest(actor=actor))

    def suspend_actor(self, atespace, name):
        return self._stub().SuspendActor(
            ateapi_pb2.SuspendActorRequest(actor=_ref(name, atespace))).actor

    def resume_actor(self, atespace, name):
        return self._stub().ResumeActor(
            ateapi_pb2.ResumeActorRequest(actor=_ref(name, atespace))).actor

    def delete_actor(self, atespace, name):
        return self._stub().DeleteActor(ateapi_pb2.DeleteActorRequest(actor=_ref(name, atespace)))

    # ─── ActorTemplate: неизменяем, Update в ateapi нет ──────────────────
    def list_actor_templates(self, atespace):
        return self._list("ListActorTemplates", "actor_templates",
                          ateapi_pb2.ListActorTemplatesRequest(atespace=atespace))

    def get_actor_template(self, atespace, name):
        return self._stub().GetActorTemplate(
            ateapi_pb2.GetActorTemplateRequest(actor_template=_ref(name, atespace)))

    def create_actor_template(self, template):
        return self._stub().CreateActorTemplate(
            ateapi_pb2.CreateActorTemplateRequest(actor_template=template))

    def delete_actor_template(self, atespace, name):
        return self._stub().DeleteActorTemplate(
            ateapi_pb2.DeleteActorTemplateRequest(actor_template=_ref(name, atespace)))

    # ─── Worker: зеркало пода, пишет ate-controller ─────────────────────
    def list_workers(self):
        return self._list("ListWorkers", "workers", ateapi_pb2.ListWorkersRequest())

    def get_worker(self, name):
        return self._stub().GetWorker(ateapi_pb2.GetWorkerRequest(worker=_ref(name)))

    def drain_worker(self, name):
        return self._stub().DrainWorker(ateapi_pb2.DrainWorkerRequest(worker=_ref(name))).worker

    # ─── AccessPolicy: одна на скоуп, имя default ────────────────────────
    def get_global_access_policy(self):
        return self._stub().GetGlobalAccessPolicy(
            ateapi_pb2.GetGlobalAccessPolicyRequest(name=POLICY))

    def create_global_access_policy(self, policy):
        return self._stub().CreateGlobalAccessPolicy(
            ateapi_pb2.CreateGlobalAccessPolicyRequest(access_policy=policy))

    def update_global_access_policy(self, policy):
        return self._stub().UpdateGlobalAccessPolicy(
            ateapi_pb2.UpdateGlobalAccessPolicyRequest(access_policy=policy))

    def get_atespace_access_policy(self, atespace):
        return self._stub().GetAtespaceAccessPolicy(
            ateapi_pb2.GetAtespaceAccessPolicyRequest(atespace=_ref(atespace), name=POLICY))

    def create_atespace_access_policy(self, atespace, policy):
        return self._stub().CreateAtespaceAccessPolicy(
            ateapi_pb2.CreateAtespaceAccessPolicyRequest(atespace=_ref(atespace),
                                                         access_policy=policy))

    def update_atespace_access_policy(self, atespace, policy):
        return self._stub().UpdateAtespaceAccessPolicy(
            ateapi_pb2.UpdateAtespaceAccessPolicyRequest(atespace=_ref(atespace),
                                                         access_policy=policy))

    def delete_atespace_access_policy(self, atespace):
        return self._stub().DeleteAtespaceAccessPolicy(
            ateapi_pb2.DeleteAtespaceAccessPolicyRequest(atespace=_ref(atespace), name=POLICY))

    # ─── шаги ────────────────────────────────────────────────────────────
    def _list(self, rpc, field, request):
        """Все элементы List-RPC постранично (потолок страницы ateapi --
        1000) -> список сообщений из поля field ответа."""
        out = []
        while True:
            response = getattr(self._stub(), rpc)(request)
            out.extend(getattr(response, field))
            if not response.next_page_token:
                return out
            request.options.page_token = response.next_page_token

    def _stub(self):
        return ateapi_pb2_grpc.ControlStub(self.channel)


def _ref(name, atespace=""):
    """Ссылка на ресурс; atespace пуст у глобальных (Atespace, Worker)."""
    return ateapi_pb2.ObjectRef(atespace=atespace, name=name)


def _server_credentials(ca_file):
    """TLS: корень -- действующий ClusterTrustBundle; читается при создании
    канала, смена корня -- редкая операция canary."""
    with open(ca_file, "rb") as f:
        return grpc.ssl_channel_credentials(root_certificates=f.read())
