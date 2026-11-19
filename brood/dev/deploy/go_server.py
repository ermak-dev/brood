"""Go-сервер на отдельном PVC, без автоматического публичного cutover."""

import base64
import hashlib
import ipaddress
import json
from pathlib import Path
import re
import socket
import ssl
import stat
from urllib.parse import urlsplit
import tempfile
import uuid

from brood.common import config
from brood.dev.deploy.infrastructure import Infrastructure
from brood.dev.substrate import local


OWNER = {"app.kubernetes.io/managed-by": "brood-go"}
PROXMOX_SETTINGS = ("BROOD_PROXMOX_POOLS", "BROOD_PROXMOX_TOKENS")
PROXMOX_RUNTIME_SETTINGS = ("BROOD_PUBLIC_ORIGIN", "BROOD_PROXMOX_RELAY_SECRET")
ACTOR_LLM_SETTINGS = ("BROOD_ACTOR_LLM_URL", "BROOD_ACTOR_LLM_API_KEY", "BROOD_ACTOR_LLM_CA_PEM_B64")
GIT_REMOTES = "BROOD_GIT_REMOTES"
# Домен Git-идентичности акторов (#249): свойство установки без дефолта,
# сервер выводит из него `<актор>@<домен>`; форму проверяет и сам.
ACTOR_GIT_DOMAIN = "BROOD_ACTOR_GIT_DOMAIN"
# Модели Claude Code акторов (#263): основная и быстрых задач, свойства
# установки без дефолта; сервер отдаёт их гостю и проверяет форму сам.
ACTOR_MODELS = ("BROOD_ACTOR_MODEL", "BROOD_ACTOR_SMALL_MODEL")
# Канон origin из контракта сервера (docs/DEPLOY.md, «Git remotes»): https, DNS-host в нижнем регистре,
# только неявный порт 443, непустой путь без "/" в конце, без userinfo, query,
# fragment и percent-encoding. Строже сервера безопасно: отказ здесь — до сборки.
GIT_HOST_LABEL = re.compile(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?")
GIT_PATH_SEGMENT = re.compile(r"[A-Za-z0-9._~-]+")


def actor_model_environment(get):
    values = {key: get(key) or "" for key in ACTOR_LLM_SETTINGS}
    if not any(values.values()):
        return {}
    https_origin(values["BROOD_ACTOR_LLM_URL"], "BROOD_ACTOR_LLM_URL")
    key = values["BROOD_ACTOR_LLM_API_KEY"]
    if not key or any(ord(char) <= 32 or ord(char) >= 127 for char in key):
        raise RuntimeError("BROOD_ACTOR_LLM_API_KEY must contain visible ASCII characters without spaces")
    ca = values["BROOD_ACTOR_LLM_CA_PEM_B64"]
    if ca:
        try:
            pem = base64.b64decode(ca, validate=True).decode("ascii")
            ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT).load_verify_locations(cadata=pem)
        except (ValueError, UnicodeError, ssl.SSLError):
            raise RuntimeError("BROOD_ACTOR_LLM_CA_PEM_B64 must contain base64-encoded PEM certificates") from None
    return {key: value for key, value in values.items() if value}


def https_origin(raw, setting):
    try:
        if not isinstance(raw, str) or not raw.isascii() or any(c.isspace() or ord(c) < 32 for c in raw):
            raise ValueError
        url = urlsplit(raw)
        if (url.scheme != "https" or not url.hostname or url.username is not None or url.password is not None
                or url.path or "?" in raw or "#" in raw or "%" in raw or "\\" in raw
                or url.netloc.endswith(":") or not 1 <= (url.port or 443) <= 65535 or url.port == 0):
            raise ValueError
        try:
            ipaddress.ip_address(url.hostname)
        except ValueError:
            if len(url.hostname) > 253 or any(not re.fullmatch(
                    r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?", label)
                    for label in url.hostname.rstrip(".").split(".")):
                raise ValueError from None
        return url.hostname, url.port or 443
    except (ValueError, TypeError):
        raise RuntimeError(f"{setting} must contain a valid HTTPS origin without credentials, path, query or fragment") from None


def proxmox_runtime_endpoints(raw):
    try:
        pools = json.loads(raw) if raw else {}
        if not isinstance(pools, dict) or any(not isinstance(pool, dict) for pool in pools.values()):
            raise ValueError
    except (ValueError, TypeError):
        raise RuntimeError("BROOD_PROXMOX_POOLS must contain a JSON object of pools") from None
    endpoints = {pool.get("endpoint") for pool in pools.values()
                 if any(key in pool for key in ("template", "templateDigest", "capacity"))
                 and isinstance(pool.get("endpoint"), str)}
    if any(not isinstance(pool.get("endpoint"), str) for pool in pools.values()
           if any(key in pool for key in ("template", "templateDigest", "capacity"))):
        raise RuntimeError("BROOD_PROXMOX_POOLS runtime requires an HTTPS endpoint")
    return endpoints


def proxmox_egress(endpoints, resolver, setting="BROOD_PROXMOX_POOLS"):
    rules = set()
    for endpoint in sorted(endpoints):
        host, port = https_origin(endpoint, setting)
        try:
            answers = resolver(host, port, type=socket.SOCK_STREAM)
            if not answers:
                raise ValueError
            for answer in answers:
                address = ipaddress.ip_address(answer[4][0])
                checked = address.ipv4_mapped if isinstance(address, ipaddress.IPv6Address) else None
                checked = checked or address
                if (checked.is_loopback or checked.is_unspecified or checked.is_multicast
                        or checked.is_link_local or checked.is_reserved or "%" in str(address)):
                    raise ValueError
                rules.add((str(checked) + f"/{checked.max_prefixlen}", port))
        except (OSError, ValueError, TypeError, IndexError):
            raise RuntimeError(f"{setting} endpoint resolution failed or returned an unsafe address") from None
    return [{"to": [{"ipBlock": {"cidr": cidr}}], "ports": [{"port": port, "protocol": "TCP"}]}
            for cidr, port in sorted(rules)]


def visible_ascii(value):
    return isinstance(value, str) and bool(value) and all(32 < ord(char) < 127 for char in value)


def unique_keys(pairs):
    # json.loads молча оставляет последний из повторов: второй remote вытеснил бы первый.
    keys = [key for key, _ in pairs]
    if len(set(keys)) != len(keys):
        raise ValueError
    return dict(pairs)


def git_origin_host(origin):
    """Канонический HTTPS URL репозитория -> его host; иначе ValueError."""
    if not isinstance(origin, str) or len(origin.encode("utf-8")) > 1024 or not origin.startswith("https://"):
        raise ValueError
    host, slash, path = origin.removeprefix("https://").partition("/")
    labels = host.split(".")
    # Последняя метка с буквы: IPv4 в любой записи (10.0.0.5, 127.1) — не DNS-имя.
    if (not slash or len(host) > 253 or not all(GIT_HOST_LABEL.fullmatch(label) for label in labels)
            or not labels[-1][0].isalpha()):
        raise ValueError
    if any(segment in (".", "..") or not GIT_PATH_SEGMENT.fullmatch(segment) for segment in path.split("/")):
        raise ValueError
    return host


def git_remote_hosts(raw):
    """BROOD_GIT_REMOTES -> отсортированные различные hosts remotes; пусто или {} -> [].

    Ключ — канонический HTTPS URL репозитория, значение — {} или ровно
    username и token видимым ASCII. Отказ называет настройку, но не значения:
    в них токены и адреса установки."""
    if not raw:
        return []
    try:
        remotes = json.loads(raw, object_pairs_hook=unique_keys)
        if not isinstance(remotes, dict):
            raise ValueError
    except (ValueError, TypeError, RecursionError):
        raise RuntimeError(f"{GIT_REMOTES} must contain a JSON object of repository remotes "
                           "without duplicate keys") from None
    hosts = set()
    for origin, credentials in remotes.items():
        try:
            hosts.add(git_origin_host(origin))
        except ValueError:
            raise RuntimeError(f"{GIT_REMOTES} keys must be canonical HTTPS repository URLs: lowercase DNS host, "
                               "no port, credentials, query or fragment, non-empty path without trailing slash") from None
        if credentials != {} and not (isinstance(credentials, dict) and set(credentials) == {"username", "token"}
                                      and all(map(visible_ascii, credentials.values()))):
            raise RuntimeError(f"{GIT_REMOTES} values must be {{}} or a username and token "
                               "of visible ASCII characters without spaces")
    return sorted(hosts)


def actor_git_domain(raw):
    """BROOD_ACTOR_GIT_DOMAIN -> домен или ""; не DNS-имя в нижнем регистре -> отказ без значения."""
    if not raw:
        return ""
    labels = raw.split(".") if isinstance(raw, str) else [""]
    if (len(raw) > 253 or not all(GIT_HOST_LABEL.fullmatch(label) for label in labels)
            or not any(c.isalpha() for c in labels[-1])):
        raise RuntimeError(f"{ACTOR_GIT_DOMAIN} must be a lowercase DNS name")
    return raw


def actor_models(get):
    """BROOD_ACTOR_MODEL/BROOD_ACTOR_SMALL_MODEL -> заданные; не 1..256 печатаемых ASCII без пробелов -> отказ без значения."""
    models = {key: get(key) or "" for key in ACTOR_MODELS}
    for key, name in models.items():
        if name and not (isinstance(name, str) and len(name) <= 256 and all(33 <= ord(c) <= 126 for c in name)):
            raise RuntimeError(f"{key} must be at most 256 printable ASCII characters without spaces")
    return {key: name for key, name in models.items() if name}


def git_remote_egress(hosts, resolver):
    """Hosts remotes -> egress сервера на TCP 443 каждого найденного адреса.

    Частный адрес LAN допустим: host задал оператор. Loopback, link-local,
    multicast, unspecified и reserved отвергает proxmox_egress."""
    return proxmox_egress({f"https://{host}" for host in hosts}, resolver, GIT_REMOTES)


# TelegramDefaultOrigin — публичный Bot API, тот же, что у сервера по умолчанию
# (go/brood/telegram.DefaultBaseURL, #281).
TELEGRAM_API = "BROOD_TELEGRAM_API"
TELEGRAM_DEFAULT_ORIGIN = "https://api.telegram.org"


def telegram_http_origin(raw, setting=TELEGRAM_API):
    """Настройка Bot API -> (host, port). Отличие от https_origin: локальный
    сервер Bot API установки задаётся HTTP origin на loopback (#281), и такой
    адрес egress не требует. Чистая функция."""
    try:
        if not isinstance(raw, str) or not raw.isascii() or any(c.isspace() or ord(c) < 32 for c in raw):
            raise ValueError
        url = urlsplit(raw)
        if url.username is not None or url.password is not None or url.path or "?" in raw \
                or "#" in raw or "%" in raw or "\\" in raw or url.netloc.endswith(":") \
                or not 1 <= (url.port or (443 if url.scheme == "https" else 80)) <= 65535 or url.port == 0:
            raise ValueError
        if url.scheme not in ("https", "http") or not url.hostname:
            raise ValueError
        try:
            address = ipaddress.ip_address(url.hostname)
        except ValueError:
            if url.scheme != "https":
                # HTTP допустим только на loopback (localhost или адрес):
                # локальный сервер Bot API установки (#281).
                if not (url.hostname == "localhost" or url.hostname.startswith("127.")):
                    raise ValueError
            if len(url.hostname) > 253 or any(not re.fullmatch(
                    r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?", label)
                    for label in url.hostname.rstrip(".").split(".")):
                raise ValueError from None
            return url.hostname, url.port or 443
        # IP-адрес: HTTPS с любым адресом (host задал оператор), HTTP — только
        # loopback: локальный сервер Bot API установки (#281).
        if url.scheme == "https" or address.is_loopback:
            return url.hostname, url.port or (443 if url.scheme == "https" else 80)
        raise ValueError
    except (ValueError, TypeError):
        raise RuntimeError(f"{setting} must contain the public HTTPS Bot API origin "
                           "or an HTTP origin on loopback") from None


def telegram_egress(raw, resolver, setting=TELEGRAM_API):
    """Настройка Bot API (None или "" -> публичный origin по умолчанию) ->
    egress правила сервера. HTTP на loopback правил не даёт: сервер
    установки достигается внутри кластера. Правила пересчитываются каждой
    выкладкой: kindnet не умеет FQDN, и смена адреса Telegram между
    выкладками останавливает уведомления до следующей выкладки (#286,
    docs/TELEGRAM.md)."""
    raw = raw if isinstance(raw, str) and raw else TELEGRAM_DEFAULT_ORIGIN
    host, port = telegram_http_origin(raw, setting)
    try:
        if ipaddress.ip_address(host).is_loopback:
            return []
    except ValueError:
        pass
    if urlsplit(raw).scheme != "https":
        return []
    return proxmox_egress({f"https://{host}" + (f":{port}" if port != 443 else "")}, resolver, setting)


def approved_images(raw):
    try:
        images = json.loads(raw)
    except (ValueError, TypeError):
        raise RuntimeError("WORKER_POOL_IMAGES must contain deployed digest-pinned images") from None
    if (not isinstance(images, dict) or "gvisor" not in images or set(images) - {"gvisor", "microvm", "lxc"}
            or any(not isinstance(value, str) or not re.fullmatch(r"[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}", value)
                   for value in images.values())):
        raise RuntimeError("WORKER_POOL_IMAGES must contain deployed digest-pinned images")
    return images


def registry_endpoint(raw):
    try:
        address = ipaddress.IPv4Address(json.loads(raw)["IPAddress"])
        if not address.is_private or address.is_loopback or address.is_unspecified or address.is_multicast:
            raise ValueError
    except (ValueError, KeyError, TypeError):
        raise RuntimeError("cannot discover the owned internal registry endpoint") from None
    return {"url": f"http://{address}:5000", "ip": str(address)}


REVISION = "deployment.kubernetes.io/revision"
# Поток ~310 МБ в норме уходит за десятки секунд; 600 с — с запасом на медленный
# API server, но не 48 минут зависшего exec (#246).
ARTIFACT_PUBLISH_TIMEOUT = 600
UNEXPECTED_OBJECTS = "guest bootstrap artifact publish cannot choose a pod: unexpected deployment/brood-go objects"


def controller_of(meta):
    return next((owner for owner in meta.get("ownerReferences") or [] if owner.get("controller")), {})


def serving(pod):
    status = pod.get("status") or {}
    return (not pod["metadata"].get("deletionTimestamp") and status.get("phase") == "Running"
            and any(c.get("type") == "Ready" and c.get("status") == "True" for c in status.get("conditions") or []))


def artifact_pod(deployment, replicasets, pods):
    """Deployment, его ReplicaSet и поды -> имя пода для публикации артефактов.

    #246: kubectl exec deployment/brood-go выбирает под сам и попал в старый,
    уже Killing. Цель доказывается цепочкой UID владельцев, которую ведёт
    контроллер: ReplicaSet текущей ревизии Deployment (revision после
    observedGeneration — ревизия применённого шаблона) с controller-ссылкой
    на UID Deployment, под с controller-ссылкой на UID этого ReplicaSet и его
    pod-template-hash. Одна метка не доказательство: её может нести и чужой
    под. Под — Running, Ready, без deletionTimestamp; ровно один, иначе отказ."""
    try:
        meta = deployment["metadata"]
        revision = (meta.get("annotations") or {}).get(REVISION)
        if not revision or (deployment.get("status") or {}).get("observedGeneration", 0) < meta.get("generation", 1):
            raise RuntimeError("guest bootstrap artifact publish refused: deployment/brood-go is not rolled out "
                               "to an observed revision")
        current = [rs for rs in replicasets
                   if (rs["metadata"].get("annotations") or {}).get(REVISION) == revision
                   and controller_of(rs["metadata"]).get("kind") == "Deployment"
                   and controller_of(rs["metadata"]).get("uid") == meta["uid"]]
        if len(current) != 1:
            raise RuntimeError("guest bootstrap artifact publish needs exactly one ReplicaSet of "
                               f"deployment/brood-go revision {revision}, found {len(current)}")
        rs = current[0]["metadata"]
        template_hash = rs["labels"]["pod-template-hash"]
        targets = sorted(p["metadata"]["name"] for p in pods
                         if controller_of(p["metadata"]).get("kind") == "ReplicaSet"
                         and controller_of(p["metadata"]).get("uid") == rs["uid"]
                         and (p["metadata"].get("labels") or {}).get("pod-template-hash") == template_hash
                         and serving(p))
    except (AttributeError, KeyError, TypeError, ValueError):
        raise RuntimeError(UNEXPECTED_OBJECTS) from None
    if len(targets) != 1:
        raise RuntimeError("guest bootstrap artifact publish needs exactly one running ready pod of "
                           f"deployment/brood-go revision {revision}, found {len(targets)}"
                           + (": " + ", ".join(targets) if targets else ""))
    return targets[0]


def manifests(image, worker_images=None, proxy=False, registry=None, proxmox=False, proxmox_runtime=False,
              proxy_api_key=False, actor_llm_settings=(), git_remotes=False, actor_git_domain=False,
              actor_model_settings=()):
    if not isinstance(image, str) or not re.fullmatch(r"localhost:5002/brood-go@sha256:[0-9a-f]{64}", image):
        raise RuntimeError("Go deployment needs an owned registry image pinned by sha256")
    workers = approved_images(json.dumps(worker_images))
    labels = {"app": "brood-go", **OWNER}

    def obj(kind, name, spec, api="v1"):
        return {"apiVersion": api, "kind": kind,
                "metadata": {"name": name, "namespace": "brood", "labels": OWNER.copy()}, "spec": spec}

    projected = {"sources": [
        {"serviceAccountToken": {"audience": "api.ate-system.svc", "expirationSeconds": 3600, "path": "token"}},
        {"clusterTrustBundle": {"signerName": "servicedns.podcert.ate.dev/identity",
                               "labelSelector": {"matchLabels": {"podcert.ate.dev/canarying": "live"}},
                               "path": "trust-bundle.pem"}},
        {"podCertificate": {"signerName": "podidentity.podcert.ate.dev/identity", "keyType": "ECDSAP256",
                            "credentialBundlePath": "client.pem"}}]}
    env = {"WORKER_POOL_IMAGES": json.dumps(workers, sort_keys=True), "BROOD_DB_PATH": "/data/brood.db", "BROOD_ADDR": ":8443",
           "BROOD_TLS_CERT": "/var/run/brood-tls/tls.crt", "BROOD_TLS_KEY": "/var/run/brood-tls/tls.key",
           "BROOD_SECURE_COOKIES": "true", "ATEAPI_TOKEN_FILE": "/var/run/ateapi/token",
           "ATEAPI_CA_FILE": "/var/run/ateapi/trust-bundle.pem",
           "ATENET_ROUTER": "atenet-router.ate-system.svc:443",
           "ATENET_CLIENT_CERT_FILE": "/var/run/ateapi/client.pem",
           "ATENET_CLIENT_KEY_FILE": "/var/run/ateapi/client.pem",
           "ATENET_CA_FILE": "/var/run/ateapi/trust-bundle.pem",
           "ATENET_SERVER_NAME": "atenet-router.ate-system.svc",
           "KUBE_API_URL": "https://kubernetes.default.svc",
           "KUBE_API_TOKEN": "/var/run/secrets/kubernetes.io/serviceaccount/token",
           "KUBE_API_CA": "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
           # Артефакты бутстрапа песочницы (#156): выкладка публикует их в PVC
           # после rollout; сервер перечитывает манифест на каждый запрос.
           "BROOD_ARTIFACTS_DIR": "/data/artifacts"}
    if registry:
        env.update(BROOD_REGISTRY_URL=registry["url"], BROOD_REGISTRY_IMAGE_HOST=f"localhost:{local.REGISTRY_PORT}")
    container = {"name": "brood", "image": image, "args": ["serve"],
                 "ports": [{"name": "https", "containerPort": 8443}],
                 "securityContext": {"runAsNonRoot": True, "runAsUser": 65532,
                                     "allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                     "capabilities": {"drop": ["ALL"]}},
                 "env": [{"name": k, "value": v} for k, v in env.items()] + [
                     {"name": "BROOD_SESSION_SECRET", "valueFrom": {"secretKeyRef": {
                         "name": "brood-go-env", "key": "BROOD_SESSION_SECRET"}}}],
                 "volumeMounts": [{"name": name, "mountPath": path, "readOnly": readonly}
                                  for name, path, readonly in (("data", "/data", False),
                                      ("ateapi", "/var/run/ateapi", True),
                                      ("tls", "/var/run/brood-tls", True), ("tmp", "/tmp", False))],
                 "readinessProbe": {"httpGet": {"path": "/healthz", "port": "https", "scheme": "HTTPS"}},
                 "resources": {"requests": {"cpu": "100m", "memory": "128Mi"},
                               "limits": {"cpu": "2", "memory": "512Mi"}}}
    if proxy:
        container["env"].extend({"name": key, "valueFrom": {"secretKeyRef": {
            "name": "brood-go-env", "key": key}}} for key in ("CLIPROXY_URL", "CLIPROXY_MANAGEMENT_KEY"))
    if proxy_api_key:
        container["env"].append({"name": "CLIPROXY_API_KEY", "valueFrom": {"secretKeyRef": {
            "name": "brood-go-env", "key": "CLIPROXY_API_KEY"}}})
    container["env"].extend({"name": key, "valueFrom": {"secretKeyRef": {
        "name": "brood-go-env", "key": key}}} for key in ACTOR_LLM_SETTINGS if key in actor_llm_settings)
    if git_remotes:
        container["env"].append({"name": GIT_REMOTES, "valueFrom": {"secretKeyRef": {
            "name": "brood-go-env", "key": GIT_REMOTES}}})
    if actor_git_domain:
        container["env"].append({"name": ACTOR_GIT_DOMAIN, "valueFrom": {"secretKeyRef": {
            "name": "brood-go-env", "key": ACTOR_GIT_DOMAIN}}})
    container["env"].extend({"name": key, "valueFrom": {"secretKeyRef": {
        "name": "brood-go-env", "key": key}}} for key in ACTOR_MODELS if key in actor_model_settings)
    if proxmox:
        container["env"].extend({"name": key, "valueFrom": {"secretKeyRef": {
            "name": "brood-go-env", "key": key}}} for key in PROXMOX_SETTINGS)
    if proxmox_runtime:
        container["env"].extend({"name": key, "valueFrom": {"secretKeyRef": {
            "name": "brood-go-env", "key": key}}} for key in PROXMOX_RUNTIME_SETTINGS)
    deployment = obj("Deployment", "brood-go", {
        "replicas": 1, "strategy": {"type": "Recreate"}, "selector": {"matchLabels": {"app": "brood-go"}},
        "template": {"metadata": {"labels": labels}, "spec": {
            # Историческое имя — точный доверенный принципал в Substrate,
            # не Django runtime. Переименование требует миграции доверия.
            "serviceAccountName": "django", "enableServiceLinks": False,
            "securityContext": {"fsGroup": 65532}, "containers": [container],
            "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "brood-go-data"}},
                        {"name": "ateapi", "projected": projected},
                        {"name": "tls", "secret": {"secretName": "brood-go-tls"}},
                        {"name": "tmp", "emptyDir": {}}]}}}, "apps/v1")
    objects = [obj("PersistentVolumeClaim", "brood-go-data", {
                "accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "1Gi"}}}),
            obj("Service", "brood-go", {"selector": {"app": "brood-go"},
                "ports": [{"name": "https", "port": 8443, "targetPort": "https"}]}), deployment,
            obj("NetworkPolicy", "brood-go", {
                "podSelector": {"matchLabels": {"app": "brood-go"}}, "policyTypes": ["Ingress", "Egress"],
                "ingress": [{"from": [{"podSelector": {"matchLabels": {"app": "edge"}}}],
                             "ports": [{"port": 8443}]}],
                "egress": [
                    {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
                             "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
                     "ports": [{"port": 53, "protocol": "UDP"}, {"port": 53, "protocol": "TCP"}]},
                    {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "ate-system"}}}],
                     "ports": [{"port": 443}]},
                    # Service:443 маршрутизатора перенаправляется на pod:8443 до egress-проверки.
                    {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "ate-system"}},
                             "podSelector": {"matchLabels": {"app": "atenet-router"}}}],
                     "ports": [{"port": 8443, "protocol": "TCP"}]}]}, "networking.k8s.io/v1")]
    if proxy:
        objects[-1]["spec"]["egress"].append({"to": [{"podSelector": {"matchLabels": {"app": "cliproxy"}}}],
                                              "ports": [{"port": 8317}]})
    if registry:
        objects[-1]["spec"]["egress"].append({"to": [{"ipBlock": {"cidr": registry["ip"] + "/32"}}],
                                              "ports": [{"port": 5000, "protocol": "TCP"}]})
    access = json.loads((Path(__file__).parent / "manifests" / "worker-access.json").read_text())
    source_role = next(item for item in access["items"] if item["kind"] == "Role")
    pod_rules = [rule for rule in source_role["rules"]
                 if rule["apiGroups"] == [""] and rule["resources"] == ["pods"]]
    if not pod_rules:
        raise RuntimeError("worker pod diagnostic permissions are not configured")
    name = "brood-go-pod-diagnostics"
    metadata = {"name": name, "namespace": "brood", "labels": OWNER.copy()}
    role = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
            "metadata": metadata.copy(), "rules": pod_rules}
    binding = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
               "metadata": metadata.copy(),
               "subjects": [{"kind": "ServiceAccount", "name": "django", "namespace": "brood"}],
               "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name}}
    return [role, binding, *objects]


class Deploy:
    def __init__(self, cluster, get=config.get, project=config.PROJECT, resolver=None):
        self.cluster, self.get, self.project = cluster, get, Path(project)
        self.resolver = resolver or socket.getaddrinfo
        # Тег образа этой выкладки: уборка Docker (#247) его не удаляет.
        self.tag = None

    def _registry_access(self):
        code, out, _ = self.cluster.shell.output("docker", "inspect", "--format",
            "{{json .NetworkSettings.Networks.kind}}", local.REGISTRY)
        if code:
            raise RuntimeError("cannot inspect the owned internal registry")
        return registry_endpoint(out)

    def _worker_images(self):
        raw = self.get("WORKER_POOL_IMAGES")
        if raw:
            return approved_images(raw)
        fallback = self.get("WORKER_POOL_IMAGE")
        if fallback:
            return approved_images(json.dumps({"gvisor": fallback}))

        code, out, _ = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}",
            "-n", "brood", "get", "deployment", "brood-go", "-o", "json", "--request-timeout=15s")
        if code:
            raise RuntimeError("Go deployment needs WORKER_POOL_IMAGES in .env or an existing owned Go deployment")
        try:
            deployment = json.loads(out)
            if not all(deployment["metadata"].get("labels", {}).get(k) == v for k, v in OWNER.items()):
                raise RuntimeError("refusing foreign deployment/brood-go")
            container = next(c for c in deployment["spec"]["template"]["spec"]["containers"] if c["name"] == "brood")
            raw = next(e["value"] for e in container["env"] if e["name"] == "WORKER_POOL_IMAGES")
        except (AttributeError, KeyError, StopIteration, TypeError, ValueError):
            raise RuntimeError("cannot read configured WORKER_POOL_IMAGES from Go deployment") from None
        return approved_images(raw)

    def _require_owned_release_targets(self):
        for kind, name in (("deployment", "brood-go"), ("service", "brood-go"),
                           ("pvc", "brood-go-data"), ("secret", "brood-go-env"), ("secret", "brood-go-tls"),
                           ("role", "brood-go-pod-diagnostics"), ("rolebinding", "brood-go-pod-diagnostics"),
                           ("networkpolicy", "brood-go"), ("networkpolicy", "brood-go-cliproxy")):
            code, out, _ = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}",
                "get", kind, name, "-n", "brood", "--ignore-not-found", "-o", "json")
            if code:
                raise RuntimeError(f"cannot check ownership of {kind}/{name}")
            if out.strip():
                try:
                    labels = json.loads(out)["metadata"].get("labels", {})
                    owned = all(labels.get(k) == v for k, v in OWNER.items())
                except (AttributeError, KeyError, TypeError, ValueError):
                    raise RuntimeError(f"cannot check ownership of {kind}/{name}: invalid JSON") from None
                if not owned:
                    raise RuntimeError(f"refusing foreign {kind}/{name}")

    def run(self, before_apply=None):
        secret = self.get("BROOD_SESSION_SECRET")
        if not secret or len(secret) < 32:
            raise RuntimeError("Go deployment needs BROOD_SESSION_SECRET of at least 32 characters in .env")
        environment = {"BROOD_SESSION_SECRET": secret}
        proxmox = {key: self.get(key) for key in PROXMOX_SETTINGS}
        if any(proxmox.values()):
            for key, value in proxmox.items():
                if not value or not value.strip():
                    raise RuntimeError(f"{key} is required for Proxmox configuration")
            # Полный контракт JSON и CA проверяет Go; deploy читает только runtime endpoints.
            environment.update(proxmox)
        endpoints = proxmox_runtime_endpoints(proxmox["BROOD_PROXMOX_POOLS"])
        runtime_env = {key: self.get(key) for key in PROXMOX_RUNTIME_SETTINGS}
        if endpoints or any(runtime_env.values()):
            for key, value in runtime_env.items():
                if not value:
                    raise RuntimeError(f"{key} is required for Proxmox runtime configuration")
            https_origin(runtime_env["BROOD_PUBLIC_ORIGIN"], "BROOD_PUBLIC_ORIGIN")
            relay = runtime_env["BROOD_PROXMOX_RELAY_SECRET"]
            if len(relay.encode("utf-8")) < 32 or relay == secret:
                raise RuntimeError("BROOD_PROXMOX_RELAY_SECRET needs at least 32 bytes and must differ from BROOD_SESSION_SECRET")
            environment.update(runtime_env)
        pve_rules = proxmox_egress(endpoints, self.resolver)
        actor_env = actor_model_environment(self.get)
        environment.update(actor_env)
        actor_rules = proxmox_egress({actor_env["BROOD_ACTOR_LLM_URL"]}, self.resolver,
                                     "BROOD_ACTOR_LLM_URL") if actor_env else []
        git_raw = self.get(GIT_REMOTES)
        git_hosts = git_remote_hosts(git_raw)
        if git_hosts:
            # Сервер получает значение целиком: полный контракт и токены — его, не deploy.
            environment[GIT_REMOTES] = git_raw
        git_rules = git_remote_egress(git_hosts, self.resolver)
        telegram_raw = self.get(TELEGRAM_API)
        if telegram_raw:
            environment[TELEGRAM_API] = telegram_raw
        telegram_rules = telegram_egress(telegram_raw, self.resolver)
        domain = actor_git_domain(self.get(ACTOR_GIT_DOMAIN))
        if domain:
            environment[ACTOR_GIT_DOMAIN] = domain
        models = actor_models(self.get)
        environment.update(models)
        proxy_url = self.get("CLIPROXY_URL")
        model_key = self.get("CLIPROXY_API_KEY")
        if model_key:
            if not proxy_url:
                raise RuntimeError("CLIPROXY_URL is required with CLIPROXY_API_KEY")
            if any(ord(char) <= 32 or ord(char) >= 127 for char in model_key):
                raise RuntimeError("CLIPROXY_API_KEY must contain visible ASCII characters without spaces")
            environment["CLIPROXY_API_KEY"] = model_key
        if proxy_url:
            proxy_key = self.get("CLIPROXY_MANAGEMENT_KEY")
            if not proxy_key:
                raise RuntimeError("CLIPROXY_MANAGEMENT_KEY is required with CLIPROXY_URL")
            environment.update(CLIPROXY_URL=proxy_url, CLIPROXY_MANAGEMENT_KEY=proxy_key)
        infrastructure = Infrastructure(self.cluster, project=str(self.project))
        tls = infrastructure._tls()
        self.cluster.check()
        infrastructure._namespace_is_ours()
        workers = self._worker_images()
        self._require_owned_release_targets()
        api_rules = infrastructure._api_egress()
        registry = self._registry_access()
        with tempfile.TemporaryDirectory(prefix="brood-go-deploy-") as directory:
            root = Path(directory)
            metadata = root / "metadata.json"
            tag = self.tag = "localhost:5002/brood-go:build-" + uuid.uuid4().hex
            release = self.get("BROOD_RELEASE_SHA")
            if release and not re.fullmatch(r"[0-9a-f]{40}", release):
                raise RuntimeError("BROOD_RELEASE_SHA must be a full commit sha")
            version_args = ("--build-arg", f"VERSION={release}") if release else ()
            code, out, err = self.cluster.shell.output("docker", "buildx", "build", "--push", "--platform",
                "linux/amd64", *version_args, "--metadata-file", str(metadata), "-t", tag,
                "-f", str(self.project / "deploy/Dockerfile.go"), str(self.project))
            if code:
                raise RuntimeError("Go image build failed: " + (err or out)[-4000:])
            digest = json.loads(metadata.read_text()).get("containerimage.digest", "")
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                raise RuntimeError("Go image build returned invalid digest metadata")
            image = "localhost:5002/brood-go@" + digest
            objects = manifests(image, workers, proxy=bool(proxy_url), registry=registry,
                                proxy_api_key=bool(model_key), proxmox=all(key in environment for key in PROXMOX_SETTINGS),
                                proxmox_runtime=all(key in environment for key in PROXMOX_RUNTIME_SETTINGS),
                                actor_llm_settings=tuple(actor_env), git_remotes=GIT_REMOTES in environment,
                                actor_git_domain=ACTOR_GIT_DOMAIN in environment,
                                actor_model_settings=tuple(models))
            # data сохраняет владение apply ключами, включая их удаление при отключении.
            env_data = {key: base64.b64encode(value.encode("utf-8")).decode("ascii")
                        for key, value in environment.items()}
            checksum = hashlib.sha256(json.dumps(
                env_data, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()
            deployment = next(obj for obj in objects if obj["kind"] == "Deployment")
            deployment["spec"]["template"]["metadata"]["annotations"] = {"brood.dev/server-env-sha256": checksum}
            objects[-1]["spec"]["egress"].extend(
                [*api_rules, *pve_rules, *actor_rules, *git_rules, *telegram_rules])
            if proxy_url:
                objects.append({"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
                    "metadata": {"name": "brood-go-cliproxy", "namespace": "brood", "labels": OWNER.copy()},
                    "spec": {"podSelector": {"matchLabels": {"app": "cliproxy"}}, "policyTypes": ["Ingress"],
                             "ingress": [{"from": [{"podSelector": {"matchLabels": {"app": "brood-go"}}}],
                                          "ports": [{"port": 8317}]}]}})
            for name, values, field, secret_type in (("brood-go-env", env_data, "data", "Opaque"),
                                                    ("brood-go-tls", tls, "stringData", "kubernetes.io/tls")):
                objects.append({"apiVersion": "v1", "kind": "Secret", "type": secret_type,
                                "metadata": {"name": name, "namespace": "brood", "labels": OWNER.copy()},
                                field: values})
            path = root / "release.json"
            path.touch(mode=0o600)
            path.write_text(json.dumps({"apiVersion": "v1", "kind": "List", "items": objects}))
            stream = self.prepare_bootstrap_artifacts(root)
            # Повторяем проверку владения после долгой подготовки, до передачи apply.
            infrastructure._namespace_is_ours()
            self._require_owned_release_targets()
            if before_apply is not None:
                before_apply(image)
            code, _, _ = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}", "apply", "-f", str(path))
            if code:
                raise RuntimeError("Go deployment apply failed; public ingress was not changed")
            code, _, err = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}", "-n", "brood",
                                                   "rollout", "status", "deployment/brood-go", "--timeout=300s")
            if code:
                raise RuntimeError("Go deployment rollout failed: " + err[-2000:])
            self.publish_bootstrap_artifacts(stream)
            return image

    def prepare_bootstrap_artifacts(self, root):
        """Готовит поток до snapshot и apply: ошибка сборки или скачивания не начинает rollout."""
        work = root / "bootstrap-artifacts"
        work.mkdir()
        pins = json.loads((self.project / "brood/dev/pins.json").read_text())
        # Релизное дерево — git archive без собранных бинарей: машину деплоя
        # не спрашивают о кэше, оба бинаря собирает её локальный go из pins.
        # cwd shell — checkout Substrate: сборка идёт с явным cwd релиза и
        # относительным пакетом, без флага -C (go на машине может быть старше).
        # bro — клиент cmd/bro (cmd/brood — сервер). Статическая сборка под
        # гостя не зависит от компилятора C и glibc машины деплоя.
        for name, package in (("bro", "./cmd/bro"), ("brood-guest", "./cmd/brood-guest")):
            code, _, err = self.cluster.shell.output(
                "env", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64",
                "go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w",
                "-o", str(work / name), package, cwd=str(self.project / "go"))
            if code or not (work / name).is_file():
                raise RuntimeError(f"guest bootstrap artifact build failed: {name}: " + err[-2000:])
        claude_bytes = self._read_bootstrap_claude(work, pins)
        # Запись «имя NUL размер NUL байты»: ELF содержит нули, поэтому длина
        # задаётся явно, а не разделителем после байтов.
        chunks = []
        for name in ("bro", "brood-guest", "claude"):
            binary = claude_bytes if name == "claude" else (work / name).read_bytes()
            chunks.extend((name.encode() + b"\0" + str(len(binary)).encode() + b"\0", binary))
        return b"".join(chunks)

    def _read_bootstrap_claude(self, work, pins):
        incoming = self.project / ".brood-deploy-artifacts"
        try:
            directory = incoming.lstat()
        except FileNotFoundError:
            claude = self._download_bootstrap_claude(work, pins)
        except OSError:
            raise RuntimeError("invalid transferred Claude artifact") from None
        else:
            claude = incoming / "claude"
            if not stat.S_ISDIR(directory.st_mode):
                raise RuntimeError("invalid transferred Claude artifact")
            try:
                metadata = claude.lstat()
            except OSError:
                raise RuntimeError("invalid transferred Claude artifact") from None
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_nlink != 1:
                raise RuntimeError("invalid transferred Claude artifact")
        try:
            binary = claude.read_bytes()
        except OSError:
            raise RuntimeError("guest bootstrap claude artifact could not be read for SHA-256 verification") from None
        if hashlib.sha256(binary).hexdigest() != pins["CLAUDE_SHA256"]:
            raise RuntimeError("guest bootstrap claude artifact SHA-256 mismatch")
        return binary

    def _download_bootstrap_claude(self, work, pins):
        claude = work / "claude"
        # Продолжаем только свой частичный файл после тайм-аута. Ошибка HTTP,
        # неподдерживаемый Range или любой другой отказ не разрешает повтор.
        for _ in range(3):
            code, _, _ = self.cluster.shell.output("curl", "--disable", "-fsSL",
                "--proto", "=https", "--proto-redir", "=https", "--connect-timeout", "20",
                "--max-time", "150", "--continue-at", "-", "-o", str(claude),
                f"https://downloads.claude.ai/claude-code-releases/{pins['CLAUDE_VERSION']}/linux-x64/claude")
            if code == 0:
                return claude
            if code != 28:
                break
        raise RuntimeError(f"guest bootstrap claude artifact download failed (curl exit {code})")

    def publish_bootstrap_artifacts(self, stream):
        """Публикует готовый поток в PVC через brood artifacts после rollout."""
        # Под выбирается перед самым exec, без подготовки артефактов между ними.
        target = self.publish_target()
        try:
            code, _, err = self.cluster.shell.output_bytes(
                ("kubectl", f"--context={local.CONTEXT}", "-n", "brood", "exec", "-i",
                 "pod/" + target, "--", "/brood", "artifacts", "--dir", "/data/artifacts"),
                input=stream, timeout=ARTIFACT_PUBLISH_TIMEOUT)
        except TimeoutError:
            # brood artifacts заменяет каталог rename только после полного потока:
            # убитый exec оставляет на томе прежний набор либо новый целиком.
            raise RuntimeError(f"guest bootstrap artifact publish timed out after {ARTIFACT_PUBLISH_TIMEOUT} s; "
                               f"kubectl exec to pod/{target} was killed and a partial stream is never published; "
                               "rerun the deploy; in existing mode remove the brood-go-deploy lease first "
                               "(docs/DEPLOY.md, guest bootstrap artifacts)") from None
        if code:
            raise RuntimeError("guest bootstrap artifact publish failed: " + err[-2000:])

    def publish_target(self):
        """Читает Deployment, его ReplicaSet и поды; выбор — artifact_pod."""
        objects = []
        for resource in (("deployment", "brood-go"), ("replicasets", "-l", "app=brood-go"),
                         ("pods", "-l", "app=brood-go")):
            code, out, _ = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}", "-n", "brood",
                "get", *resource, "-o", "json", "--request-timeout=15s")
            if code:
                raise RuntimeError(f"guest bootstrap artifact publish cannot read {resource[0]} of deployment/brood-go")
            try:
                objects.append(json.loads(out))
            except ValueError:
                raise RuntimeError(UNEXPECTED_OBJECTS) from None
        deployment, replicasets, pods = objects
        try:
            return artifact_pod(deployment, replicasets["items"], pods["items"])
        except (KeyError, TypeError):
            raise RuntimeError(UNEXPECTED_OBJECTS) from None
