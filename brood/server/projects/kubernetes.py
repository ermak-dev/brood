"""Namespaced-клиент Kubernetes только для WorkerPool; без kubeconfig и kubectl.

CA проверяется TLS, токен ServiceAccount перечитывается на каждый запрос.
Редиректов нет: Bearer не должен уехать на другой сервер.
"""
import http.client
import json
import re
import ssl
from pathlib import Path
from urllib.parse import urlencode, urlsplit

NAMESPACE = "brood"
COLLECTION = f"/apis/ate.dev/v1/namespaces/{NAMESPACE}/workerpools"
NAME = re.compile(r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?\Z")


class KubeError(Exception):
    """Отказ Kubernetes с кодом HTTP и безопасной причиной для REST-клиента."""

    def __init__(self, status, detail):
        super().__init__(detail)
        self.status = status


def valid_name(name):
    if not isinstance(name, str) or len(name) > 253 or not all(
        len(label) <= 63 and NAME.fullmatch(label) for label in name.split(".")
    ):
        raise KubeError(400, "pool name must be a lowercase DNS subdomain up to 253 characters, with labels up to 63 characters")
    return name


class Client:
    """Клиент ограниченного ресурса; настройки и транспорт не зависят от Django."""

    def __init__(self, endpoint, token_file, ca_file):
        try:
            url = urlsplit(endpoint)
            if url.scheme != "https" or not url.hostname or url.username or url.password or url.path not in ("", "/") or url.query or url.fragment:
                raise ValueError("not an HTTPS origin")
            self.host, self.port = url.hostname, url.port or 443
        except ValueError:
            raise KubeError(503, "KUBE_API_URL must be an HTTPS origin with a valid port") from None
        self.token_file, self.ca_file = token_file, ca_file

    def list(self):
        items, token = [], ""
        while True:
            query = {"limit": 500}
            if token:
                query["continue"] = token
            response = self._request("GET", COLLECTION + "?" + urlencode(query))
            if not isinstance(response.get("items"), list):
                raise KubeError(502, "Kubernetes returned an invalid WorkerPool list")
            items.extend(response["items"])
            token = response.get("metadata", {}).get("continue", "")
            if not token:
                return items

    def get(self, name):
        return self._request("GET", self._item(name))

    def create(self, body):
        return self._request("POST", COLLECTION, body)

    def update(self, name, body):
        return self._request("PUT", self._item(name), body)

    def delete(self, name, preconditions):
        return self._request("DELETE", self._item(name), {
            "apiVersion": "v1", "kind": "DeleteOptions", "preconditions": preconditions,
            "propagationPolicy": "Foreground",
        })

    def _item(self, name):
        return COLLECTION + "/" + valid_name(name)

    def _request(self, method, path, body=None):
        try:
            token = Path(self.token_file).read_text().strip()
            context = ssl.create_default_context(cafile=self.ca_file)
        except (OSError, ValueError) as e:
            raise KubeError(503, "Kubernetes credentials unavailable: check KUBE_API_TOKEN and KUBE_API_CA") from e
        if not token:
            raise KubeError(503, "KUBE_API_TOKEN is empty")
        headers = {"Authorization": f"Bearer {token}", "Accept": "application/json"}
        payload = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            payload = json.dumps(body).encode()
        conn = http.client.HTTPSConnection(self.host, self.port, context=context, timeout=10)
        try:
            conn.request(method, path, body=payload, headers=headers)
            response = conn.getresponse()
            raw = response.read()
        except (OSError, http.client.HTTPException) as e:
            raise KubeError(503, "Kubernetes API connection failed; check its address, CA and network policy") from e
        finally:
            conn.close()
        try:
            data = json.loads(raw)
        except (ValueError, UnicodeError):
            raise KubeError(502, f"Kubernetes returned HTTP {response.status} without a JSON response") from None
        if not isinstance(data, dict):
            raise KubeError(502, "Kubernetes returned a non-object response")
        if not 200 <= response.status < 300:
            status = response.status if response.status in (400, 401, 403, 404, 409, 422, 429) else 502
            raise KubeError(status, data.get("message") or f"Kubernetes returned HTTP {response.status}")
        return data
