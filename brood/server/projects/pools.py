"""WorkerPool установки: REST к Kubernetes с ограниченным набором полей.

WorkerPool порождает привилегированные поды. Даже namespaced RBAC не
позволяет принимать произвольные workerImage, ServiceAccount и pod-аннотации.
"""
import copy
import re

from django.conf import settings
from rest_framework.response import Response
from rest_framework.views import APIView

from brood.server.projects import kubernetes
from brood.server.projects.views import ApiError, SessionSuperuser, only


def client_of():
    values = (settings.KUBE_API_URL, settings.KUBE_API_TOKEN, settings.KUBE_API_CA)
    if not all(values):
        raise ApiError(503, "Kubernetes is not configured: KUBE_API_URL, KUBE_API_TOKEN and KUBE_API_CA are required")
    return call(kubernetes.Client, *values)


def call(method, *args):
    try:
        return method(*args)
    except kubernetes.KubeError as e:
        raise ApiError(e.status, str(e)) from None


def image():
    value = settings.WORKER_POOL_IMAGE
    if not value or not re.fullmatch(r"[^\s@]+@sha256:[0-9a-f]{64}", value):
        raise ApiError(503, "WORKER_POOL_IMAGE must be the deployed digest-pinned worker image")
    return value


def object_at(value, field):
    if not isinstance(value, dict):
        raise ApiError(400, f"{field} must be a JSON object")
    return value


def fields(value, allowed, field):
    object_at(value, field)
    extra = value.keys() - set(allowed)
    if extra:
        raise ApiError(400, f"{field}: unsupported fields: {', '.join(sorted(extra))}")


def preconditions(data):
    metadata = object_at(object_at(data, "body").get("metadata"), "metadata")
    result = {}
    for key in ("uid", "resourceVersion"):
        value = metadata.get(key)
        if not isinstance(value, str) or not value.strip():
            raise ApiError(400, f"metadata.{key} from the loaded pool is required; reload before retrying")
        result[key] = value
    return result


def string_map(value, field):
    object_at(value, field)
    if not all(isinstance(k, str) and isinstance(v, str) for k, v in value.items()):
        raise ApiError(400, f"{field} must contain string keys and values")


def validate_spec(spec):
    fields(spec, ("replicas", "workerImage", "sandboxClasses", "template"), "spec")
    replicas = spec.get("replicas")
    if type(replicas) is not int or not 0 <= replicas <= 2147483647:
        raise ApiError(400, "spec.replicas must be an integer between 0 and 2147483647")
    if spec.get("workerImage") != image():
        raise ApiError(400, "spec.workerImage must match the deployed worker image")
    classes = spec.get("sandboxClasses")
    if not isinstance(classes, list) or len(classes) != 1:
        raise ApiError(400, "spec.sandboxClasses must contain exactly one gvisor class")
    fields(classes[0], ("name", "configRef"), "spec.sandboxClasses[0]")
    if classes[0].get("name") != "gvisor":
        raise ApiError(400, "only the gvisor sandbox class is supported")
    if "configRef" in classes[0]:
        ref = classes[0]["configRef"]
        fields(ref, ("name",), "configRef")
        if not isinstance(ref.get("name"), str) or not ref["name"].strip():
            raise ApiError(400, "configRef.name is required")
    template = spec.get("template", {})
    fields(template, ("resources", "labels", "nodeSelector"), "spec.template")
    for key in ("labels", "nodeSelector"):
        if key in template:
            string_map(template[key], f"spec.template.{key}")
    resources = template.get("resources", {})
    fields(resources, ("requests", "limits"), "spec.template.resources")
    for key, quantities in resources.items():
        fields(quantities, ("cpu", "memory"), f"resources.{key}")
        if not all(isinstance(v, str) and v.strip() for v in quantities.values()):
            raise ApiError(400, f"resources.{key} quantities must be nonempty strings")


def body(data, name=None):
    object_at(data, "body")
    metadata = object_at(data.get("metadata", {}), "metadata")
    for unsafe in ("ownerReferences", "finalizers"):
        if unsafe in metadata:
            raise ApiError(400, f"metadata.{unsafe} cannot be changed through this API")
    name = call(kubernetes.valid_name, name if name is not None else metadata.get("name"))
    identity = {"name": name, "namespace": kubernetes.NAMESPACE}
    for key in ("labels", "annotations"):
        if key in metadata:
            string_map(metadata[key], f"metadata.{key}")
            identity[key] = copy.deepcopy(metadata[key])
    spec = object_at(data.get("spec"), "spec")
    validate_spec(spec)
    return {"apiVersion": "ate.dev/v1", "kind": "WorkerPool",
            "metadata": identity, "spec": copy.deepcopy(spec)}


class PoolDefaultsView(APIView):
    permission_classes = [SessionSuperuser]

    def get(self, _request):
        return Response({"workerImage": image()})


class PoolView(APIView):
    permission_classes = [SessionSuperuser]

    def get(self, _request, name=None):
        api = client_of()
        if name is not None:
            call(kubernetes.valid_name, name)
            return Response(call(api.get, name))
        return Response({"results": call(api.list)})

    def post(self, request, name=None):
        only(request, name is None)
        data = body(request.data)
        return Response(call(client_of().create, data), status=201)

    def put(self, request, name=None):
        only(request, name is not None)
        conditions = preconditions(request.data)
        data = body(request.data, name)
        data["metadata"].update(conditions)
        return Response(call(client_of().update, name, data))

    def delete(self, request, name=None):
        only(request, name is not None)
        call(kubernetes.valid_name, name)
        conditions = preconditions(request.data)
        call(client_of().delete, name, conditions)
        return Response(status=204)
