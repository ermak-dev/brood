"""Чистый разбор Broodfile и проверка входов плана без процессов и сети."""

import copy
import ipaddress
import json
from pathlib import Path, PurePosixPath
import re
from urllib.parse import unquote, urlsplit

from jsonschema import Draft202012Validator
import yaml
from yaml.events import AliasEvent
from yaml.nodes import MappingNode, ScalarNode, SequenceNode


def _fail(filename, field, reason, line=None):
    location = f"{filename}: {field}"
    if line is not None:
        location += f" (line {line})"
    raise RuntimeError(f"{location}: {reason}") from None


def _field(path):
    return ".".join(str(part) if re.fullmatch(r"[A-Za-z0-9_-]{1,64}", str(part))
                    else "[field]" for part in path) or "document"


def _control(value):
    return any(ord(char) < 32 or 127 <= ord(char) <= 159 for char in value)


class _Loader(yaml.SafeLoader):
    """Ограничивает глубину до конструирования объектов и не допускает ссылок."""

    def __init__(self, text, filename):
        super().__init__(text)
        self.filename = filename
        self.depth = 0

    def compose_node(self, parent, index):
        event = self.peek_event()
        if isinstance(event, AliasEvent) or getattr(event, "anchor", None) is not None:
            _fail(self.filename, "document", "YAML anchors and aliases are forbidden",
                  event.start_mark.line + 1)
        if self.depth >= 64:
            _fail(self.filename, "document", "nesting limit exceeded", event.start_mark.line + 1)
        self.depth += 1
        try:
            return super().compose_node(parent, index)
        finally:
            self.depth -= 1


def _check_nodes(node, filename, locations, path=()):
    locations[path] = node.start_mark.line + 1
    tags = {"null", "bool", "int", "float", "str", "seq", "map"}
    if node.tag not in {f"tag:yaml.org,2002:{tag}" for tag in tags}:
        _fail(filename, _field(path), "unsupported YAML tag", node.start_mark.line + 1)
    if isinstance(node, MappingNode):
        seen = set()
        for key, value in node.value:
            if not isinstance(key, ScalarNode) or key.tag != "tag:yaml.org,2002:str":
                _fail(filename, _field(path), "mapping keys must be strings", key.start_mark.line + 1)
            if key.value == "<<":
                _fail(filename, _field(path), "YAML merge keys are forbidden", key.start_mark.line + 1)
            if key.value in seen:
                _fail(filename, _field(path), "duplicate mapping key", key.start_mark.line + 1)
            seen.add(key.value)
            _check_nodes(value, filename, locations, (*path, key.value))
    elif isinstance(node, SequenceNode):
        for index, value in enumerate(node.value):
            _check_nodes(value, filename, locations, (*path, index))


def _validate_schema(document, filename, locations=None):
    schema = json.loads(Path(__file__).with_name("schema.json").read_text())
    try:
        error = next(Draft202012Validator(schema).iter_errors(document), None)
    except (RecursionError, TypeError, ValueError):
        _fail(filename, "document", "invalid document structure")
    if error is not None:
        path = tuple(error.absolute_path)
        line = None
        if locations:
            lookup = path
            while lookup not in locations and lookup:
                lookup = lookup[:-1]
            line = locations.get(lookup)
        # Сообщения jsonschema включают исходные значения и не выходят наружу.
        _fail(filename, _field(path), f"schema constraint failed ({error.validator})", line)


def parse_document(text, filename="Broodfile"):
    """Возвращает независимый словарь после безопасного YAML-разбора и схемы."""
    if not isinstance(text, str):
        _fail(filename, "document", "expected text")
    try:
        size = len(text.encode("utf-8"))
    except UnicodeError:
        _fail(filename, "document", "invalid UTF-8 text")
    if size > 1024 * 1024:
        _fail(filename, "document", "document exceeds 1 MiB")
    loader = None
    try:
        loader = _Loader(text, filename)
        node = loader.get_single_node()
        locations = {}
        if node is None:
            _fail(filename, "document", "empty document", 1)
        _check_nodes(node, filename, locations)
        document = loader.construct_document(node)
    except yaml.YAMLError as error:
        mark = getattr(error, "problem_mark", None)
        _fail(filename, "document", "invalid YAML", mark.line + 1 if mark else 1)
    except (RecursionError, ValueError, OverflowError):
        _fail(filename, "document", "invalid YAML structure", 1)
    finally:
        if loader is not None:
            loader.dispose()
    _validate_schema(document, filename, locations)
    return document


def safe_path(value, field="path"):
    """Нормализует относительный POSIX-путь без выхода из контекста."""
    if (not isinstance(value, str) or not value or _control(value) or "\\" in value
            or value.startswith("/") or re.match(r"^[A-Za-z]:", value)
            or ".." in value.split("/")):
        raise RuntimeError(f"{field}: expected a safe relative POSIX path")
    result = str(PurePosixPath(value))
    if result == ".":
        raise RuntimeError(f"{field}: empty or dot path is forbidden")
    return result


def _dns_name(value):
    return (isinstance(value, str) and len(value) <= 253
            and all(re.fullmatch(r"[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?", label)
                    for label in value.split(".")))


def public_hostname(value):
    """Проверяет точное публичное DNS-имя без локальных и специальных суффиксов."""
    if not _dns_name(value) or "." not in value:
        raise RuntimeError("hostname: expected an exact public DNS name")
    host = value.lower()
    private = {"localhost", "local", "localdomain", "internal", "intranet", "lan", "home", "corp", "private",
               "test", "invalid", "example", "onion", "arpa", "svc", "cluster"}
    if host.rsplit(".", 1)[1] in private or host.rsplit(".", 1)[1].isdigit():
        raise RuntimeError("hostname: private or special DNS suffix is forbidden")
    try:
        ipaddress.ip_address(host)
    except ValueError:
        return host
    raise RuntimeError("hostname: IP addresses are forbidden")


def _url_host(value):
    if not value:
        return False
    try:
        ipaddress.ip_address(value)
        return True
    except ValueError:
        return bool(_dns_name(value))


def repository(value):
    """Принимает только Git URL без credentials, query, fragment и опасных схем."""
    message = "repository: expected a credential-free HTTPS, SSH or git@host:path URL"
    if (not isinstance(value, str) or not value or _control(value)
            or any(char.isspace() for char in value) or "\\" in value):
        raise RuntimeError(message)
    try:
        if value.startswith(("https://", "ssh://")):
            url = urlsplit(value)
            if (not _url_host(url.hostname) or url.password is not None
                    or (url.scheme == "https" and url.username is not None)
                    or url.query or url.fragment or "?" in value or "#" in value
                    or not url.path or url.path == "/"
                    or (url.port is not None and not 1 <= url.port <= 65535)):
                raise ValueError
            if url.scheme == "ssh" and url.username is not None:
                if not re.fullmatch(r"[A-Za-z0-9_-]+", url.username):
                    raise ValueError
            safe_path(unquote(url.path.lstrip("/")))
        else:
            match = re.fullmatch(r"git@([^:/]+):(.+)", value)
            if not match or not _url_host(match[1]) or "?" in value or "#" in value:
                raise ValueError
            safe_path(unquote(match[2]))
    except (ValueError, RuntimeError):
        raise RuntimeError(message) from None
    return value


def _registry(value):
    if (not isinstance(value, str) or not value or _control(value)
            or any(char.isspace() for char in value) or any(char in value for char in "@?#\\")):
        raise RuntimeError("registry: expected a credential-free OCI registry prefix")
    parts = value.split("/")
    try:
        endpoint = urlsplit("//" + parts[0])
        if (not _url_host(endpoint.hostname) or endpoint.username is not None
                or (endpoint.port is not None and not 1 <= endpoint.port <= 65535)
                or parts[0].endswith(":")):
            raise ValueError
        for part in parts[1:]:
            if not re.fullmatch(r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*", part):
                raise ValueError
    except ValueError:
        raise RuntimeError("registry: expected a credential-free OCI registry prefix") from None
    return value


def _checked(function, value, filename, field):
    try:
        return function(value)
    except RuntimeError as error:
        _fail(filename, field, str(error))


def _pin_values(pins, filename):
    if pins is None:
        return {}
    if not isinstance(pins, dict):
        _fail(filename, "pins", "expected an object of string pins")
    for name, value in pins.items():
        if not isinstance(name, str) or not re.fullmatch(r"[A-Z][A-Z0-9_]*", name):
            _fail(filename, "pins", "invalid pin name")
        if not isinstance(value, str) or not value or len(value) > 16384 or _control(value):
            _fail(filename, "pins", "expected nonempty bounded string pin values without controls")
    return copy.deepcopy(pins)


def _pin(name, pins, pins_file, filename, field):
    if pins_file is None:
        _fail(filename, field, "pin reference requires pinsFile")
    if name not in pins:
        _fail(filename, field, "referenced pin is missing")
    return pins[name]


def _reference(name, definitions, filename, field):
    if name not in definitions:
        _fail(filename, field, "unknown reference")


def _sources(document, pins, pins_file, filename):
    result = {}
    destinations = []
    projects = 0
    for name, source in document["sources"].items():
        field = f"sources.{name}"
        normalized = copy.deepcopy(source)
        destination = _checked(safe_path, source["destination"], filename, field + ".destination")
        for existing in destinations:
            if (destination == existing or destination.startswith(existing + "/")
                    or existing.startswith(destination + "/")):
                _fail(filename, field + ".destination", "source destinations overlap")
        destinations.append(destination)
        normalized["destination"] = destination
        normalized.setdefault("gitMetadata", False)
        if source["type"] == "project":
            projects += 1
        else:
            normalized["repository"] = _checked(repository, source["repository"], filename, field + ".repository")
            if "revisionPin" in source:
                revision = _pin(source["revisionPin"], pins, pins_file, filename, field + ".revisionPin")
                if not re.fullmatch(r"[0-9a-f]{40}", revision):
                    _fail(filename, field + ".revisionPin", "expected a full lowercase 40-hex commit pin")
                normalized.pop("revisionPin")
                normalized["revision"] = revision
        result[name] = normalized
    if projects > 1:
        _fail(filename, "sources", "at most one project source is allowed")
    return result


def _builds(document, sources, pins, pins_file, filename):
    result = {}
    for name, environment in document["environments"].items():
        field = f"environments.{name}"
        for source in environment["sources"]:
            _reference(source, sources, filename, field + ".sources")
        build = environment["build"]
        dockerfile = _checked(safe_path, build["dockerfile"], filename, field + ".build.dockerfile")
        if not any(dockerfile.startswith(sources[source]["destination"] + "/")
                   for source in environment["sources"]):
            _fail(filename, field + ".build.dockerfile", "Dockerfile must be under a selected source")
        arguments = copy.deepcopy(build.get("arguments", {}))
        from_pins = build.get("argsFromPins", [])
        for key in [*arguments, *from_pins]:
            if any(word in key for word in ("TOKEN", "PASSWORD", "SECRET", "PRIVATE_KEY", "AUTH")):
                _fail(filename, field + ".build", "secret-like build argument names are forbidden")
        for key in from_pins:
            if key in arguments:
                _fail(filename, field + ".build", "duplicate build argument")
            arguments[key] = _pin(key, pins, pins_file, filename, field + ".build.argsFromPins")
        result[name] = {"dockerfile": dockerfile, "platform": build["platform"], "arguments": arguments}
    return result


def _unique(values, filename, field):
    if len(values) != len(set(values)):
        _fail(filename, field, "duplicate names or paths")


def _actor(actor, filename, field):
    for key, value in actor["resources"]["limits"].items():
        if not 0 < int(value) <= 2**64 - 1:
            _fail(filename, field + ".resources.limits." + key, "resource must fit uint64")
    volumes = [volume["name"] for volume in actor.get("volumes", [])]
    _unique(volumes, filename, field + ".volumes")
    for container in actor["containers"]:
        _unique([entry["name"] for entry in container.get("env", [])], filename, field + ".containers.env")
        mounts = container.get("volumeMounts", [])
        _unique([entry["name"] for entry in mounts], filename, field + ".containers.volumeMounts")
        paths = []
        for mount in mounts:
            _reference(mount["name"], volumes, filename, field + ".containers.volumeMounts.name")
            path = mount["mountPath"]
            if "\\" in path or _control(path) or ".." in path.split("/"):
                _fail(filename, field + ".containers.volumeMounts.mountPath", "unsafe absolute mount path")
            paths.append(str(PurePosixPath(path)))
        _unique(paths, filename, field + ".containers.volumeMounts.mountPath")


def _definitions(document, filename):
    for name, network in document["networks"].items():
        for protocol, rule in network.items():
            for host in rule["hosts"]:
                _checked(public_hostname, host, filename, f"networks.{name}.{protocol}.hosts")
    for name, template in document["templates"].items():
        field = f"templates.{name}"
        _reference(template["environment"], document["environments"], filename, field + ".environment")
        _reference(template["network"], document["networks"], filename, field + ".network")
        _actor(template["actor"], filename, field + ".actor")
    for name, target in document["targets"].items():
        _reference(target["template"], document["templates"], filename, f"targets.{name}.template")
    _reference(document["defaultTarget"], document["targets"], filename, "defaultTarget")


def make_plan(document, pins=None, target=None, registry=None, atespace=None, filename="Broodfile"):
    """Проверяет весь документ и возвращает независимый JSON-план выбранной цели."""
    _validate_schema(document, filename)
    pins = _pin_values(pins, filename)
    pins_file = document.get("pinsFile")
    if pins_file is not None:
        pins_file = _checked(safe_path, pins_file, filename, "pinsFile")
    sources = _sources(document, pins, pins_file, filename)
    builds = _builds(document, sources, pins, pins_file, filename)
    _definitions(document, filename)
    if target is None:
        target = document["defaultTarget"]
    if not isinstance(target, str):
        _fail(filename, "target", "expected target name")
    _reference(target, document["targets"], filename, "target")
    if not registry:
        _fail(filename, "registry", "BUILD_REGISTRY is required")
    registry = _checked(_registry, registry, filename, "registry")
    if not atespace:
        _fail(filename, "atespace", "BUILD_ATESPACE is required")
    if (not isinstance(atespace, str) or len(atespace) > 63
            or not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]*[a-z0-9])?", atespace)):
        _fail(filename, "atespace", "BUILD_ATESPACE must be a DNS label")
    template = document["templates"][document["targets"][target]["template"]]
    environment = template["environment"]
    selected = document["environments"][environment]
    used_pins = set(selected["build"].get("argsFromPins", []))
    for source in selected["sources"]:
        revision_pin = document["sources"][source].get("revisionPin")
        if revision_pin is not None:
            used_pins.add(revision_pin)
    return {
        "apiVersion": document["apiVersion"], "project": document["name"], "target": target,
        "environment": environment,
        "sources": {name: sources[name] for name in selected["sources"]},
        "build": builds[environment], "actor": copy.deepcopy(template["actor"]),
        "network": copy.deepcopy(document["networks"][template["network"]]),
        "registry": registry, "atespace": atespace,
        "pins": {key: pins[key] for key in sorted(used_pins)}, "pinsFile": pins_file,
    }
