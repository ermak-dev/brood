"""Трекер и CI проекта: issues и пайплайны GitLab.

Модуль возвращает данные и ничего не печатает; печатают `brood dev bug` и
`brood dev ci` (brood/dev/cli).

Координаты берутся из origin рабочей копии brood, а не из настройки: проект
в трекере и проект в git -- один проект, и второе место правды однажды
разойдётся с первым. В gateway-клоне локальный brood.origin хранит канонический
origin, а remote origin остаётся транспортом. `.env` переопределяет
(`GITLAB_SITE`, `GITLAB_PROJECT`),
когда трекер живёт не там, где код.

Секрет один: `GITLAB_TOKEN`, личный токен с правом api, из `.env`.
"""
import contextlib
import contextvars
import hashlib
import http.client
import json
import os
import re
import subprocess
import tempfile
import urllib.error
import urllib.parse
import urllib.request

from brood.common import config

TIMEOUT = 20

# Словарь меток. Группы -- scoped labels GitLab: у задачи ровно одна из
# каждой, вторая молча вытесняет первую. Значения проверяются здесь, а не в
# GitLab: опечатка в метке заводит новую метку, и задача пропадает из выборки.
VOCAB = {
    "status": ("live", "wip", "fixed", "noise", "dup", "parked"),
    "sev": ("high", "med", "low"),
    "component": ("bus", "nomad", "agent", "session", "puppets", "driver",
                  "deploy", "mcp", "master", "cli", "docs"),
    "type": ("bug", "feature", "refactor", "docs", "ci", "epic"),
}
# Незакрытые состояния: задача в очереди (live), взята в работу (wip) либо
# отложена (parked). Отложенная открыта, но не в работе: закрытая отложенная
# пропадает из `brood dev bug list` и выглядит сделанной.
OPEN_STATUSES = ("live", "wip", "parked")
CLOSE_STATUSES = ("fixed", "noise", "dup")
SEV_ORDER = {"sev::high": 0, "sev::med": 1, "sev::low": 2}

# Типы, дающие префикс ветки: префикс -- слово метки как есть (CLAUDE.md,
# «Ветка задачи»). Сокращение feature до feat расходилось с веткой, которую
# исполнитель заводит по правилу, и заметка называла ветку, которой нет (#297).
# Эпика здесь нет: `epic/` -- интеграционные ветки, а не ветки задач.
TYPES = ("bug", "feature", "refactor", "docs", "ci")


# ─── координаты ──────────────────────────────────────────────────────────
def origin():
    """Канонический origin рабочей копии brood. Пусто -- координат нет."""
    return _checkout_origin(config.PROJECT)


def _checkout_origin(checkout):
    """Локальный brood.origin гостевого клона, иначе transport origin (#302)."""
    try:
        result = subprocess.run(["git", "-C", checkout, "config", "--local", "--get", "brood.origin"],
                                capture_output=True, text=True)
        if result.returncode == 0:
            return result.stdout.strip()
        if result.returncode != 1:
            return ""
        return subprocess.run(["git", "-C", checkout, "remote", "get-url", "origin"],
                              capture_output=True, text=True, check=True).stdout.strip()
    except (subprocess.CalledProcessError, FileNotFoundError):
        return ""


_SCHEME = re.compile(r"^([A-Za-z][A-Za-z0-9+.-]*)://")


def parse_origin(url):
    """origin -> (хост, путь проекта); ("", ""), если координат GitLab нет.

    Формы те же, что у git: scheme://[user@]host[:port]/path и scp-форма
    [user@]host:path. Порт, схема и хвостовой .git -- не часть пути проекта.
    Слеш до двоеточия -- локальный путь, хоста у него нет."""
    s = (url or "").strip()
    m = _SCHEME.match(s)
    if m:
        netloc, _, path = s[m.end():].partition("/")
        host = netloc.rpartition("@")[2].partition(":")[0]
    elif ":" in s and "/" not in s.split(":", 1)[0]:
        left, _, path = s.partition(":")
        host = left.rpartition("@")[2]
    else:
        return "", ""
    path = path.removesuffix("/").removesuffix(".git")
    return (host, path) if host and path else ("", "")


def site():
    url = origin()
    host = parse_origin(url)[0]
    try:
        parsed = urllib.parse.urlsplit(url) if _SCHEME.match(url) else None
        if parsed and parsed.scheme.lower() in ("http", "https") and parsed.port:
            default = 80 if parsed.scheme.lower() == "http" else 443
            if parsed.port != default:
                host += f":{parsed.port}"
    except ValueError:
        host = ""
    s = config.get("GITLAB_SITE") or host
    if not s:
        raise RuntimeError("no GitLab site: set GITLAB_SITE in .env or add a git origin")
    return s


_project_scope = contextvars.ContextVar("gitlab_project", default=None)


@contextlib.contextmanager
def project_scope(path):
    """Ограничить путь или ID одним вызовом; вложенные вызовы восстанавливают их."""
    if not isinstance(path, str) or not re.fullmatch(
            r"(?:[1-9][0-9]*|[A-Za-z0-9_][A-Za-z0-9_.-]*(?:/[A-Za-z0-9_][A-Za-z0-9_.-]*)+)", path):
        raise RuntimeError("invalid GitLab project: expected group/project or a positive numeric ID")
    token = _project_scope.set(path)
    try:
        yield
    finally:
        _project_scope.reset(token)


def require_local_project():
    """Выбрать совпадающий worktree из cwd, иначе настроенную рабочую копию.

    Если ни одна не совпадает с координатами трекера, отказать до записи.
    """
    result = subprocess.run(["git", "rev-parse", "--show-toplevel"],
                            capture_output=True, text=True)
    candidates = ([result.stdout.strip()] if result.returncode == 0 else []) + [config.PROJECT]
    try:
        target = urllib.parse.urlsplit("https://" + site())
        target_port = target.port if target.port is not None else 443
        selected_project = project()
        for checkout in candidates:
            url = _checkout_origin(checkout)
            host, path = parse_origin(url)
            parsed = urllib.parse.urlsplit(url) if _SCHEME.match(url) else None
            scheme = parsed.scheme.lower() if parsed else "ssh"
            if parsed:
                if parsed.netloc.endswith(":"):
                    continue
                parsed.port  # Проверить порт даже у SSH, хотя к API он не относится.
            if scheme in ("http", "https"):
                port = parsed.port if parsed.port is not None else (80 if scheme == "http" else 443)
                same_site = scheme == target.scheme and port == target_port
            else:
                # Порт SSH относится к git, а не к API.
                same_site = target_port == 443 and scheme == "ssh"
            if (same_site and host.lower() == (target.hostname or "").lower()
                    and path and path == selected_project):
                return checkout
    except ValueError:
        pass
    raise RuntimeError("local git origin does not match the selected GitLab project")


def project():
    p = _project_scope.get() or config.get("GITLAB_PROJECT") or parse_origin(origin())[1]
    if not p:
        raise RuntimeError("no GitLab project: set GITLAB_PROJECT in .env or add a git origin")
    return p


def base():
    return f"https://{site()}/api/v4/projects/{urllib.parse.quote(project(), safe='')}"


# ─── запросы ─────────────────────────────────────────────────────────────
def auth_header():
    """Чем представляться GitLab: личный токен. Другого способа нет: OAuth-грант
    по паролю GitLab убрал в 19.0, и отказ здесь называет настройку, которую
    надо завести, а не код ответа из сети."""
    pat = config.get("GITLAB_TOKEN")
    if not pat:
        raise RuntimeError("no GitLab credentials in .env: GITLAB_TOKEN, a personal "
                           "access token with the api scope")
    return ("PRIVATE-TOKEN", pat)


def call(method, path, payload=None, params=None):
    """Запрос к API проекта, ответ -- разобранный JSON."""
    raw = request(method, base() + path, path, payload, params)
    return json.loads(raw) if raw else None


def call_site(method, path, payload=None, params=None):
    """Запрос к API инстанса, мимо проекта: /runners/<id> у GitLab не под
    проектом, хотя раннер проекту и назначен."""
    raw = request(method, f"https://{site()}/api/v4" + path, path, payload, params)
    return json.loads(raw) if raw else None


def _read_response(response, method, path):
    """Чтение обоих видов ответа: обрыв не доказывает отказ записи (#301)."""
    try:
        return response.read()
    except (http.client.IncompleteRead, OSError):
        raise RuntimeError(f"{method} {path}: incomplete response; tracker outcome not confirmed") from None


def request(method, url, path, payload=None, params=None):
    """Запрос как есть -> тело байтами: трасса джобы -- текст, не JSON.
    path -- чем назвать запрос в отказе: полный URL в нём был бы шумом.
    Отказ HTTP и сети -- RuntimeError строкой с причиной."""
    url += ("?" + urllib.parse.urlencode(params)) if params else ""
    req = urllib.request.Request(
        url, method=method,
        data=json.dumps(payload).encode() if payload is not None else None)
    req.add_header(*auth_header())
    if payload is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            return _read_response(r, method, path)
    except urllib.error.HTTPError as e:
        with e:
            _read_response(e, method, path)
        # Сервер может повторить секрет запроса в теле ошибки: не печатаем его.
        raise RuntimeError(f"{method} {path} -> HTTP {e.code}; tracker outcome not confirmed") from None
    except urllib.error.URLError as e:
        raise RuntimeError(f"{method} {path}: {e.reason}") from None
    except http.client.IncompleteRead:
        raise RuntimeError(f"{method} {path}: incomplete response; tracker outcome not confirmed") from None
    except TimeoutError:
        raise RuntimeError(f"{method} {path}: no answer in {TIMEOUT}s") from None


def _paged(path, params):
    out, page = [], 1
    while True:
        batch = call("GET", path, params={**params, "per_page": 100, "page": page})
        out += batch
        if len(batch) < 100:
            return out
        page += 1


# ─── задачи ──────────────────────────────────────────────────────────────
def issues(state="opened", labels=(), search=None):
    """Задачи как данные, тяжёлые первыми: сначала sev, потом номер."""
    params = {"state": state, "order_by": "created_at"}
    if labels:
        params["labels"] = ",".join(labels)
    if search:
        params["search"] = search
    return sorted(_paged("/issues", params), key=sev_key)


def issue(iid):
    return call("GET", f"/issues/{iid}")


def create(title, body, labels):
    return call("POST", "/issues", {"title": title, "description": body,
                                    "labels": ",".join(labels)})


def comment(iid, body):
    return call("POST", f"/issues/{iid}/notes", {"body": body})


def relabel(iid, labels):
    return call("PUT", f"/issues/{iid}", {"labels": ",".join(labels)})


def edit(iid, title=None, body=None):
    """Заголовок и/или тело задачи. Что не названо, не трогается: в запросе
    нет такого поля вовсе, а не пустое значение."""
    return call("PUT", f"/issues/{iid}", edit_payload(title, body))


def close(iid, labels):
    return call("PUT", f"/issues/{iid}", {"labels": ",".join(labels),
                                          "state_event": "close"})


def reopen(iid, labels):
    return call("PUT", f"/issues/{iid}", {"labels": ",".join(labels),
                                          "state_event": "reopen"})


def children(epic_iid):
    """Задачи эпика: те, у кого в первой строке тела стоит его маркер.

    Выборкой по телу, а не по связям GitLab: связи -- платная редакция, а
    маркер виден и в вебе, и в выводе `brood dev bug show`."""
    return [i for i in issues(state="all") if epic_of(i.get("description")) == int(epic_iid)]


# ─── пайплайны, джобы, раннеры ───────────────────────────────────────────
# Ещё идущие: у GitLab это и created/pending/running, и промежуточные
# waiting_for_resource/preparing.
PIPELINE_WAIT = ("created", "waiting_for_resource", "preparing", "pending", "running")


def pipelines(n=15):
    """Последние n пайплайнов проекта, новые первыми."""
    return call("GET", "/pipelines", params={"order_by": "id", "sort": "desc",
                                             "per_page": n})


def pipeline_by_id(pid):
    """Пайплайн целиком: yaml_errors есть только здесь, не в списке."""
    return call("GET", f"/pipelines/{int(pid)}")


def jobs(pid):
    """Джобы пайплайна в порядке стадий. Перезапущенные копии GitLab сам
    прячет: показывается последняя попытка, та, что решает цвет."""
    return sorted(_paged(f"/pipelines/{int(pid)}/jobs", {}), key=lambda j: j["id"])


def job(jid):
    return call("GET", f"/jobs/{int(jid)}")


def trace(jid):
    """Лог джобы текстом, как его показал бы терминал: у GitLab это не
    JSON, а сырой вывод раннера с \\r секций."""
    raw = request("GET", base() + f"/jobs/{int(jid)}/trace", f"/jobs/{int(jid)}/trace")
    return screen_lines(raw.decode(errors="replace")) if raw else ""


def _artifact_endpoint(jid, path):
    jid = str(jid).lstrip("0")
    if not re.fullmatch(r"[1-9][0-9]*", jid):
        raise RuntimeError("invalid job ID: expected a positive decimal integer")
    if (not isinstance(path, str) or re.search(r"[\x00-\x1f\x7f-\x9f\\]", path)
            or any(part in ("", ".", "..") for part in path.split("/"))):
        raise RuntimeError("invalid artifact path: expected a relative file path without dot segments")
    try:
        return f"/jobs/{jid}/artifacts/{urllib.parse.quote(path, safe='/')}"
    except UnicodeEncodeError:
        raise RuntimeError("invalid artifact path: expected UTF-8 text") from None


def _artifact_output(output):
    try:
        output = os.fspath(output)
    except TypeError:
        raise RuntimeError("invalid output: expected a file path") from None
    if (not isinstance(output, str) or "\x00" in output
            or os.path.basename(output) in ("", ".", "..")):
        raise RuntimeError("invalid output: expected a file path")
    try:
        parent = os.path.realpath(os.path.dirname(output) or ".", strict=True)
    except (OSError, ValueError):
        raise RuntimeError("invalid output: destination directory is unavailable") from None
    output = os.path.join(parent, os.path.basename(output))
    if os.path.lexists(output):
        raise RuntimeError("output already exists")
    return output


class _NoArtifactRedirect(urllib.request.HTTPRedirectHandler):
    def http_error_302(self, req, fp, code, msg, headers):
        # Передаём отказ стандартному обработчику, не разбирая Location.
        return None

    http_error_301 = http_error_303 = http_error_307 = http_error_308 = http_error_302


def _open_artifact(endpoint):
    try:
        req = urllib.request.Request(base() + endpoint, method="GET")
        req.add_unredirected_header(*auth_header())
        return urllib.request.build_opener(_NoArtifactRedirect()).open(req, timeout=TIMEOUT)
    except urllib.error.HTTPError as error:
        with error:
            # Тело и Location могут содержать секреты; читаем только код.
            reason = ": redirect refused" if 300 <= error.code < 400 else ""
            raise RuntimeError(f"GitLab artifact request returned HTTP {error.code}{reason}") from None
    except (OSError, http.client.HTTPException, ValueError):
        raise RuntimeError("GitLab artifact request failed") from None


def _stream_artifact(response, scratch, digest):
    if response.status != 200:
        raise RuntimeError(f"GitLab artifact request returned HTTP {response.status}")
    sizes = response.headers.get_all("Content-Length", [])
    length = sizes[0].strip() if sizes else None
    if sizes and (len(sizes) != 1 or not re.fullmatch(r"[0-9]+", length)):
        raise RuntimeError("GitLab artifact has an invalid Content-Length")
    if length is None and digest is None:
        raise RuntimeError("GitLab artifact has no Content-Length; provide --sha256")
    received = 0
    while True:
        try:
            chunk = response.read(64 * 1024)
        except (OSError, http.client.HTTPException, ValueError):
            raise RuntimeError("incomplete response while downloading GitLab artifact") from None
        if not chunk:
            break
        scratch.write(chunk)
        received += len(chunk)
        if digest is not None:
            digest.update(chunk)
    # Ограниченный read() может вернуть EOF без ошибки при коротком теле.
    if length is not None and (length.lstrip("0") or "0") != str(received):
        raise RuntimeError("incomplete response while downloading GitLab artifact")


def download_artifact(jid, path, output, sha256=None):
    """Скачать один файл по ID джобы; опубликовать только целый файл."""
    endpoint = _artifact_endpoint(jid, path)
    output = _artifact_output(output)
    if sha256 is not None and (not isinstance(sha256, str) or not re.fullmatch(r"[0-9a-fA-F]{64}", sha256)):
        raise RuntimeError("invalid SHA-256: expected 64 hexadecimal digits")
    digest = hashlib.sha256() if sha256 is not None else None
    try:
        scratch = tempfile.NamedTemporaryFile(dir=os.path.dirname(output), prefix=".brood-artifact-", delete=False)
        try:
            with scratch, _open_artifact(endpoint) as response:
                _stream_artifact(response, scratch, digest)
            if digest is not None and digest.hexdigest() != sha256.lower():
                raise RuntimeError("GitLab artifact SHA-256 mismatch")
            # link публикует закрытый файл атомарно и не заменяет даже симлинк.
            os.link(scratch.name, output)
        finally:
            with contextlib.suppress(FileNotFoundError):
                os.unlink(scratch.name)
    except FileExistsError:
        raise RuntimeError("output already exists") from None
    except OSError:
        raise RuntimeError("could not write artifact output; check the destination directory and free space") from None


def lint(content):
    """POST /ci/lint проекта: include в .gitlab-ci.yml разрешаются от него."""
    return call("POST", "/ci/lint", {"content": content, "include_jobs": True})


def retry_job(jid):
    return call("POST", f"/jobs/{int(jid)}/retry")


def retry_pipeline(pid):
    return call("POST", f"/pipelines/{int(pid)}/retry")


def cancel_pipeline(pid):
    return call("POST", f"/pipelines/{int(pid)}/cancel")


def runners():
    """Раннеры проекта с метками. Список метки не несёт -- только карточка
    раннера, а она у GitLab не под проектом."""
    return [call_site("GET", f"/runners/{int(r['id'])}")
            for r in _paged("/runners", {})]


def running_jobs():
    """Идущие сейчас джобы проекта, каждая -- с раннером, который её держит."""
    return _paged("/jobs", {"scope[]": "running"})


VARIABLE_NAME = re.compile(r"^[A-Za-z0-9_]{1,255}$")


def variables():
    """Переменные CI/CD проекта без значений: значение секрета не должно
    доехать до вывода даже по ошибке командлета."""
    return [{k: v for k, v in var.items() if k != "value"}
            for var in _paged("/variables", {})]


def set_variable(key, value, file=False):
    """Завести или обновить переменную CI/CD -> "created" | "updated".

    Всегда protected -- её видят только конвейеры защищённых веток -- и raw:
    без raw GitLab раскрыл бы $NAME внутри значения. Маски нет: многострочное
    значение GitLab замаскировать не даёт."""
    if not VARIABLE_NAME.match(key):
        raise RuntimeError(f"bad CI variable name {key!r}: letters, digits and _ only")
    payload = {"value": value, "variable_type": "file" if file else "env_var",
               "protected": True, "raw": True, "masked": False}
    if key in {var["key"] for var in variables()}:
        call("PUT", f"/variables/{key}", payload)
        return "updated"
    call("POST", "/variables", {"key": key, **payload})
    return "created"


# ─── чистое: метки, эпики, ветки ─────────────────────────────────────────
def sev_key(i):
    return (SEV_ORDER.get(scoped(i.get("labels") or [], "sev::"), 3), i["iid"])


def scoped(labels, prefix):
    return next((x for x in labels if x.startswith(prefix)), "")


def check_label(name):
    """Метка из словаря либо громкий отказ с перечнем. Опечатка в scoped-метке
    не ошибка для GitLab: он заведёт новую метку, и задача просто выпадет из
    выборок, которыми её ищут."""
    group, _, value = name.partition("::")
    if group not in VOCAB:
        raise RuntimeError(f"unknown label group {group}; one of: {', '.join(VOCAB)}")
    if value not in VOCAB[group]:
        raise RuntimeError(f"unknown {group} '{value}'; one of: {', '.join(VOCAB[group])}")
    return name


# Эпик -- не сущность GitLab (она в платной редакции), а маркер первой строкой
# тела. Дёшево, видно человеку в вебе и не требует прав сверх обычной задачи.
_EPIC = re.compile(r"^\*\*Эпик:\*\*\s*#(\d+)\s*$", re.M)


def epic_of(body):
    """Родительский эпик задачи либо None. Считается только первая строка:
    ссылка на эпик где-то в теле -- это ссылка, а не родство."""
    first = (body or "").lstrip().splitlines()[:1]
    m = _EPIC.match(first[0]) if first else None
    return int(m.group(1)) if m else None


def with_epic(body, iid):
    """Тело задачи с маркером эпика первой строкой. Идемпотентно, и старый
    маркер заменяется: два маркера означали бы двух родителей."""
    body = (body or "").lstrip()
    if epic_of(body) is not None:
        rest = body.splitlines()[1:]
        body = "\n".join(rest).lstrip("\n")
    marker = f"**Эпик:** #{iid}"
    return f"{marker}\n\n{body}" if body else marker


# Маркер эпика при правке тела: оставить старый, поставить новый, снять.
# Сентинел, а не None: None -- это и есть «снять».
KEEP = object()


def replace_body(old, new, epic=KEEP):
    """Новое тело задачи вместо старого с учётом родства. Маркер эпика живёт
    первой строкой тела, и правка тела целиком молча уводила бы задачу
    из-под эпика. Новое тело со своим маркером -- как есть; без него -- под
    старым родителем, если epic не сказал иначе: число ставит другой эпик,
    None снимает."""
    new = (new or "").strip()
    if epic is None:
        return bare_body(new)
    if epic is not KEEP:
        return with_epic(new, epic)
    if epic_of(new) is not None:
        return new
    parent = epic_of(old)
    return with_epic(new, parent) if parent is not None else new


def bare_body(body):
    """Тело без маркера эпика первой строкой."""
    if epic_of(body) is None:
        return body
    return "\n".join(body.splitlines()[1:]).lstrip("\n")


def edit_payload(title, body):
    """Поля запроса правки: только названные. Ничего не названо -- ValueError:
    пустая правка означала бы ошибку в аргументах, а не «ничего не менять»."""
    payload = {}
    if title:
        payload["title"] = title
    if body is not None:
        payload["description"] = body
    if not payload:
        raise ValueError("nothing to edit: name --title or --body-file")
    return payload


def slugify(s):
    """ASCII-слаг для имени ветки: не-ASCII выбрасывается, не транслитерируется
    -- заголовки русские, а имена веток остаются английскими."""
    return re.sub(r"[^a-z0-9]+", "-", s.lower()).strip("-")[:40].rstrip("-")


def branch_type(labels):
    """Префикс ветки по меткам задачи. Дефект перевешивает: задача не должна
    нести и `bug`, и тип, но если несёт -- правда о работе в дефекте."""
    if "bug" in labels or "type::bug" in labels:
        return "bug"
    for kind in TYPES:
        if kind in labels or f"type::{kind}" in labels:
            return kind
    return "feature"


def branch_name(labels, iid, slug=None):
    clean = slugify(slug) if slug else ""
    return f"{branch_type(labels)}/{iid}" + (f"-{clean}" if clean else "")


def with_status(existing, status):
    """Метки после смены состояния: снять любую `status::` и поставить одну.
    Идемпотентно: двух меток одной группы быть не должно, GitLab молча
    оставит одну и не скажет какую."""
    allowed = OPEN_STATUSES + CLOSE_STATUSES
    if status not in allowed:
        raise RuntimeError(f"unknown status '{status}'; one of: {', '.join(allowed)}")
    return [x for x in existing if not x.startswith("status::")] + [f"status::{status}"]


def relabel_action(state, status):
    """Чем `brood dev bug relabel` меняет задачу: "reopen" либо "relabel".

    Открытый статус на закрытой задаче открывает её: иначе задача с меткой
    очереди молча сидит среди закрытых и пропадает из `brood dev bug list`.
    Закрывает только `brood dev bug close`, с комментарием."""
    return "reopen" if status in OPEN_STATUSES and state == "closed" else "relabel"


# ─── чистое: CI ──────────────────────────────────────────────────────────
# Метка -- слово, а не значок: вывод читает модель, а у неё нет глаз, чтобы
# отличить зелёную галку от красного крестика. Провал -- капсом: его и ищут.
MARKS = {"success": "ok", "failed": "FAILED"}
# Законченные: отменять нечего. GitLab на отмену такого отвечает успехом и
# не делает ничего, а молчаливое «сделано» хуже отказа.
PIPELINE_DONE = ("success", "failed", "canceled", "skipped")


def mark(status):
    return MARKS.get(status, status or "?")


def duration(secs):
    """Секунды GitLab -> "-" / "Ns" / "NmSSs". null -- джоба не стартовала,
    и ноль там был бы враньём."""
    if secs is None:
        return "-"
    secs = int(secs)
    return f"{secs}s" if secs < 60 else f"{secs // 60}m{secs % 60:02d}s"


def tail(text, n):
    """Последние n строк; None -- всё. Перевод строки в конце трассы строкой
    не считается: иначе «последние 60» были бы 59."""
    lines = (text or "").rstrip("\n").splitlines()
    return "\n".join(lines if n is None else lines[-n:] if n > 0 else [])


def screen_lines(text):
    """Трасса как её видит терминал: от строки -- то, что после последнего \\r.
    GitLab пишет секции как «section_start:<t>:<имя>\\r\\x1b[0K<текст>»,
    прогресс -- через \\r; без этого хвост лога -- мусор маркеров. \\r\\n --
    просто конец строки. Цвета остаются: их снимает тот, кто печатает."""
    lines = (text or "").replace("\r\n", "\n").split("\n")
    return "\n".join(line.rsplit("\r", 1)[-1] for line in lines)


def is_failure(job):
    """Провал, который валит пайплайн: failed без allow_failure. Терпимый
    провал цвет не решает, и «почему красный» не должен звать его причиной."""
    return job.get("status") == "failed" and not job.get("allow_failure")


def _wait_refusal(kind, obj):
    status = obj.get("status")
    if status in PIPELINE_WAIT:
        return f"{kind} {obj.get('id')} is {status}: wait for it to finish before retrying"
    return None


def retry_blocked(job):
    """Отказ повтора идущей джобы либо None. GitLab ответил бы 403 без
    объяснения, и искать пришлось бы по коду ответа."""
    return _wait_refusal("job", job)


def pipeline_retry_blocked(p):
    return _wait_refusal("pipeline", p)


def pipeline_cancel_blocked(p):
    """Отказ отмены законченного пайплайна либо None: GitLab принял бы её
    молча и ничего не сделал."""
    status = p.get("status")
    if status in PIPELINE_DONE:
        return f"pipeline {p.get('id')} is {status}: nothing to cancel"
    return None


def pipeline_failure(p):
    """Причина провала от самого пайплайна -> [строка]. Отвергнутый
    .gitlab-ci.yml валит пайплайн без единой джобы: смотреть на джобы тогда
    бесполезно, причина лежит в пайплайне."""
    out = []
    if p.get("yaml_errors"):
        out.append(f"yaml_errors: {p['yaml_errors']}")
    if p.get("failure_reason"):
        out.append(f"failure_reason: {p['failure_reason']}")
    text = (p.get("detailed_status") or {}).get("text")
    if text:
        out.append(f"detailed_status: {text}")
    return out


def _when(stamp):
    """2026-09-24T10:11:12.345Z -> 2026-09-24 10:11 (UTC, как отдаёт GitLab)."""
    return stamp[:16].replace("T", " ") if stamp else "-"


def pipeline_line(p):
    return (mark(p.get("status")), str(p["id"]), (p.get("sha") or "")[:8] or "-",
            p.get("ref") or "-", _when(p.get("created_at")), p.get("web_url") or "-")


def job_line(j):
    """Строка джобы. Терпимый провал -- не капсом: пайплайн он не валил."""
    allowed = j.get("status") == "failed" and j.get("allow_failure")
    return ("failed" if allowed else mark(j.get("status")), str(j["id"]),
            j.get("stage") or "-", j.get("name") or "-", duration(j.get("duration")),
            "(allow_failure)" if j.get("allow_failure") else "")


def runner_line(r):
    """ON/OFF -- принимает ли раннер работу; status -- видит ли его GitLab.
    Разные вещи: остановленный раннер бывает online, и джобы тогда стоят."""
    on = r.get("active", True) and not r.get("paused", False)
    return ("ON" if on else "OFF", str(r["id"]), r.get("description") or "-",
            r.get("status") or "-", ",".join(r.get("tag_list") or []) or "-")


def running_line(j):
    runner = j.get("runner") or {}
    held = (f"runner {runner['id']} {runner.get('description') or ''}".rstrip()
            if runner.get("id") else "no runner")
    return (str(j["id"]), j.get("stage") or "-", j.get("name") or "-",
            j.get("ref") or "-", held)


def lint_report(result):
    """Ответ /ci/lint -> ([строка], годен ли)."""
    if result.get("valid"):
        names = [j.get("name", "?") for j in result.get("jobs") or []]
        lines = [f"valid, {len(names)} job(s): {', '.join(names)}"]
    else:
        lines = ["invalid"] + [f"error: {e}" for e in result.get("errors") or []]
    lines += [f"warning: {w}" for w in result.get("warnings") or []]
    return lines, bool(result.get("valid"))


def why_lines(p, jobs, tails):
    """Почему пайплайн такой, какой есть -> [строка].

    jobs -- джобы пайплайна, tails -- {id джобы: хвост трассы} провалившихся.
    Терпимые провалы названы отдельно и прямо: иначе их красный цвет в вебе
    принимают за причину. Без провалившихся джоб -- причина самого
    пайплайна: отвергнутый .gitlab-ci.yml джоб не заводит вовсе."""
    status = p.get("status")
    out = [f"pipeline {p['id']} {status}: {p.get('web_url')}"]
    failed = [j for j in jobs if is_failure(j)]
    tolerated = [j for j in jobs if j.get("status") == "failed" and j.get("allow_failure")]
    for j in failed:
        out += ["", f"job {j['id']} {j.get('stage')}/{j.get('name')} failed "
                    f"({duration(j.get('duration'))}): {j.get('web_url')}"]
        text = tails.get(j["id"])
        out += text.splitlines() if text else ["(empty trace)"]
    for j in tolerated:
        out += ["", f"job {j['id']} {j.get('stage')}/{j.get('name')} failed "
                    f"(allow_failure): did NOT fail the pipeline"]
    if failed:
        return out
    if status == "failed":
        reasons = pipeline_failure(p)
        return out + (["no jobs failed; the pipeline itself says:"] + reasons if reasons
                      else ["no jobs failed, and GitLab gives no reason"])
    return out if tolerated else out + ["nothing failed"]
