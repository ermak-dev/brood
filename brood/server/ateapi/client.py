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
        self.channel = grpc.secure_channel(
            endpoint,
            grpc.composite_channel_credentials(
                _server_credentials(ca_file),
                grpc.metadata_call_credentials(BearerToken(token_file))))

    def close(self):
        self.channel.close()

    # ─── Atespace ────────────────────────────────────────────────────────
    def list_atespaces(self, page_token=""):
        """Все atespace постранично -> [ateapi_pb2.Atespace]."""
        out = []
        while True:
            request = ateapi_pb2.ListAtespacesRequest(
                options=ateapi_pb2.ListOptions(page_token=page_token))
            response = self._stub().ListAtespaces(request)
            out.extend(response.atespaces)
            page_token = response.next_page_token
            if not page_token:
                return out

    def get_atespace(self, name):
        return self._stub().GetAtespace(
            ateapi_pb2.GetAtespaceRequest(atespace=ateapi_pb2.ObjectRef(name=name)))

    def create_atespace(self, name):
        return self._stub().CreateAtespace(
            ateapi_pb2.CreateAtespaceRequest(
                atespace=ateapi_pb2.Atespace(
                    metadata=ateapi_pb2.ResourceMetadata(name=name))))

    def delete_atespace(self, name):
        return self._stub().DeleteAtespace(
            ateapi_pb2.DeleteAtespaceRequest(
                atespace=ateapi_pb2.ObjectRef(name=name)))

    # ─── Actor ───────────────────────────────────────────────────────────
    def list_actors(self, atespace, page_token=""):
        """Все акторы atespace постранично -> [ateapi_pb2.Actor]."""
        out = []
        while True:
            request = ateapi_pb2.ListActorsRequest(
                atespace=atespace,
                options=ateapi_pb2.ListOptions(page_token=page_token))
            response = self._stub().ListActors(request)
            out.extend(response.actors)
            page_token = response.next_page_token
            if not page_token:
                return out

    def get_actor(self, atespace, name):
        return self._stub().GetActor(
            ateapi_pb2.GetActorRequest(actor=self._ref(atespace, name)))

    def suspend_actor(self, atespace, name):
        return self._stub().SuspendActor(
            ateapi_pb2.SuspendActorRequest(actor=self._ref(atespace, name))).actor

    def resume_actor(self, atespace, name):
        return self._stub().ResumeActor(
            ateapi_pb2.ResumeActorRequest(actor=self._ref(atespace, name))).actor

    def delete_actor(self, atespace, name):
        return self._stub().DeleteActor(
            ateapi_pb2.DeleteActorRequest(actor=self._ref(atespace, name)))

    def _ref(self, atespace, name):
        """Ссылка на ресурс: atespace + имя."""
        return ateapi_pb2.ObjectRef(atespace=atespace, name=name)

    def _stub(self):
        return ateapi_pb2_grpc.ControlStub(self.channel)


def _server_credentials(ca_file):
    """TLS: корень -- действующий ClusterTrustBundle; читается при создании
    канала, смена корня -- редкая операция canary."""
    with open(ca_file, "rb") as f:
        return grpc.ssl_channel_credentials(root_certificates=f.read())
