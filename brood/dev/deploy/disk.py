"""Место на машине деплоя (#247): preflight свободного места и уборка Docker.

Выкладка собирает образы сервера и edge через buildx на этой же машине, и
кэш сборки без предела заполнил корневой раздел вместе с томом базы. Пределы —
свойства машины (размер диска, том базы), поэтому они настройки .env без
дефолта: без настройки шаг пропускается, как до #247, а командлет говорит об
этом предупреждением. Библиотека возвращает данные и молчит.
"""
import datetime
import json
import re

from brood.dev.substrate import local


MIN_FREE = "BROOD_DEPLOY_MIN_FREE_GB"
BUILD_CACHE = "BROOD_DEPLOY_BUILD_CACHE_GB"
KEEP_SNAPSHOTS = "BROOD_DEPLOY_KEEP_SNAPSHOTS"
# GB настроек — 2^30 байт, как у `df -h` и `--keep-storage` Docker.
GIB = 1 << 30
# Образы, которые собирает сама выкладка (go_server.py, public.py): тег
# build-<uuid4 hex>. Образы kind, registry, ateworker и чужие теги не наши.
REPOSITORIES = (f"localhost:{local.REGISTRY_PORT}/brood-go", f"localhost:{local.REGISTRY_PORT}/brood-go-edge")
BUILD_TAG = re.compile(r"build-[0-9a-f]{32}")
DF_LINE = re.compile(r"(?P<fs>\S.*?)\s+\d+\s+\d+\s+(?P<available>\d+)\s+(?:\d+%|-)\s+(?P<mount>/.*)")
USAGE_VALUE = re.compile(r"[0-9A-Za-z.%() ]{1,32}")
NOTHING_REMOVED = "nothing was removed"


def whole(get, name, minimum):
    """Настройка -> None (не задана) или целое не меньше minimum; иное — отказ с именем."""
    raw = get(name) or ""
    if not raw:
        return None
    if not (raw.isascii() and raw.isdecimal() and len(raw) <= 6) or int(raw) < minimum:
        raise RuntimeError(f"{name} must be a whole number of at least {minimum}")
    return int(raw)


def settings(get):
    """Форма всех настроек места — до доступа к машине и кластеру."""
    return {MIN_FREE: whole(get, MIN_FREE, 1), BUILD_CACHE: whole(get, BUILD_CACHE, 0),
            KEEP_SNAPSHOTS: whole(get, KEEP_SNAPSHOTS, 1)}


def gib(size):
    return f"{size / GIB:.1f} GiB"


def available(df):
    """Вывод `df -P -k ПУТЬ` -> (свободно байт, точка монтирования)."""
    lines = df.strip().splitlines()
    match = DF_LINE.fullmatch(lines[1].strip()) if len(lines) == 2 else None
    if not match:
        raise RuntimeError(f"cannot read free space of the Docker root directory for the {MIN_FREE} check; "
                           "nothing was built or changed")
    return int(match["available"]) * 1024, match["mount"]


def usage(system_df):
    """Вывод `docker system df --format '{{json .}}'` -> кэш сборки и образы для причины отказа."""
    found = {}
    for line in system_df.splitlines():
        try:
            item = json.loads(line)
        except ValueError:
            continue
        if not isinstance(item, dict) or item.get("Type") not in {"Build Cache", "Images"}:
            continue
        size, spare = item.get("Size"), item.get("Reclaimable")
        if all(isinstance(v, str) and USAGE_VALUE.fullmatch(v) for v in (size, spare)):
            found[item["Type"]] = f"{size} (reclaimable {spare})"
    return "; ".join(f"{label} {found[kind]}" for kind, label in (("Build Cache", "build cache"),
                                                                   ("Images", "images")) if kind in found)


def verdict(df, limit, system_df=""):
    """df раздела Docker, предел GiB и (для отказа) docker system df -> вердикт или отказ."""
    free, mount = available(df)
    if free < limit * GIB:
        known = usage(system_df)
        raise RuntimeError(f"free space on {mount} (Docker root) is {gib(free)}, below {MIN_FREE}={limit} GiB"
                           + (f"; {known}" if known else "")
                           + "; nothing was built or changed; free space first, for example "
                           "docker builder prune -f --keep-storage 5GB as the deploy user, and set "
                           f"{BUILD_CACHE} so that every deploy prunes the build cache (docs/DEPLOY.md)")
    return {"checked": True, "mount": mount, "free": free, "limit": limit * GIB}


def space(shell, get):
    """Preflight: свободное место раздела каталога данных Docker (образы, кэш
    сборки, узел kind и том базы на нём). Без MIN_FREE — {"checked": False}."""
    limit = whole(get, MIN_FREE, 1)
    if limit is None:
        return {"checked": False}
    code, root, _ = shell.output("docker", "info", "--format", "{{.DockerRootDir}}")
    root = root.strip()
    if code or not root.startswith("/"):
        raise RuntimeError(f"cannot find the Docker root directory for the {MIN_FREE} check; "
                           "nothing was built or changed")
    code, df, _ = shell.output("df", "-P", "-k", root)
    if code:
        raise RuntimeError(f"cannot read free space of the Docker root directory for the {MIN_FREE} check; "
                           "nothing was built or changed")
    system_df = ""
    if available(df)[0] < limit * GIB:
        # docker system df обходит тома: медленно, поэтому только ради причины отказа.
        code, out, _ = shell.output("docker", "system", "df", "--format", "{{json .}}")
        system_df = "" if code else out
    return {**verdict(df, limit, system_df), "root": root}


def moment(created):
    """CreatedAt `docker image ls` ("2026-11-10 03:09:32 +0700 +07") -> datetime."""
    return datetime.datetime.strptime(" ".join(created.split()[:3]), "%Y-%m-%d %H:%M:%S %z")


def stale_images(listing, current):
    """Вывод `docker image ls --format '{{json .}}'` и теги этой выкладки -> (удалить, оставить).

    На репозиторий остаются тег этой выкладки и один тег самого нового иного
    образа — предыдущего. Остальные свои теги удаляются по ссылке repo:tag, не
    по ID: тег того же образа, что текущий (edge из кэша), только снимается.
    Нечитаемый список или неизвестный текущий тег — отказ до первого удаления."""
    images = {repo: {} for repo in REPOSITORIES}
    try:
        for line in filter(str.strip, listing.splitlines()):
            item = json.loads(line)
            repo, name = item["Repository"], item["Tag"]
            if repo in images and isinstance(name, str) and BUILD_TAG.fullmatch(name):
                images[repo][name] = (moment(item["CreatedAt"]), item["ID"])
    except (ValueError, KeyError, TypeError, AttributeError):
        raise RuntimeError(f"cannot read the local Docker image list; {NOTHING_REMOVED}") from None
    remove, keep = [], []
    for repo, tags in images.items():
        mine = [ref.rpartition(":")[2] for ref in current
                if isinstance(ref, str) and ref.rpartition(":")[0] == repo]
        if len(mine) != 1 or not BUILD_TAG.fullmatch(mine[0]):
            raise RuntimeError(f"the image of this deploy in {repo} is unknown; {NOTHING_REMOVED}")
        if mine[0] not in tags:
            raise RuntimeError(f"the image of this deploy {repo}:{mine[0]} is not in the local Docker image list; "
                               + NOTHING_REMOVED)
        ident = tags[mine[0]][1]
        older = sorted(((created, name) for name, (created, other) in tags.items() if other != ident), reverse=True)
        kept = [mine[0]] + [name for _, name in older[:1]]
        keep += [f"{repo}:{name}" for name in kept]
        remove += [f"{repo}:{name}" for name in sorted(tags) if name not in kept]
    return remove, keep


def reclaimed(output):
    """Итог `docker builder prune` ("Total:" buildx, "Total reclaimed space:" классический)."""
    found = re.search(r"^Total[^:\n]*:\s*(\S+)", output, re.MULTILINE)
    return found[1] if found else ""


def tail(text):
    return " ".join((text or "").split())[-300:]


def attempt(shell, *argv):
    """shell.output, где сбой запуска процесса (нет бинаря, ENOMEM) - код 1 с причиной."""
    try:
        return shell.output(*argv)
    except (RuntimeError, OSError) as error:
        return 1, "", str(error)


def tidy(shell, get, current):
    """Уборка после успешной выкладки: старые теги своих образов, затем кэш
    сборки до BUILD_CACHE GiB. Сбой шага — запись в errors, не отказ: выкладка
    уже прошла, а следующую остановит preflight места. Без BUILD_CACHE — ничего."""
    limit = whole(get, BUILD_CACHE, 0)
    if limit is None:
        return {"enabled": False}
    result = {"enabled": True, "cache": limit, "removed": [], "kept": [], "reclaimed": "", "errors": []}
    try:
        code, listing, err = attempt(shell, "docker", "image", "ls", "--format", "{{json .}}")
        if code:
            raise RuntimeError(f"cannot list local Docker images: {tail(err)}; {NOTHING_REMOVED}")
        remove, result["kept"] = stale_images(listing, current)
    except RuntimeError as error:
        result["errors"].append(f"old release images: {error}")
        remove = []
    for ref in remove:
        code, _, err = attempt(shell, "docker", "image", "rm", ref)
        if code:
            result["errors"].append(f"old release images: docker image rm {ref} failed: {tail(err)}")
        else:
            result["removed"].append(ref)
    # --keep-storage принимают классический `docker builder prune` (Debian 13) и buildx; в
    # buildx 0.17+ это устаревшее имя --reserved-space, которое без других пределов тоже
    # сокращает кэш до N. Именно эта команда освободила 56 ГБ на машине деплоя 2026-11-10.
    code, out, err = attempt(shell, "docker", "builder", "prune", "--force", "--keep-storage", f"{limit}GB")
    if code:
        result["errors"].append(f"build cache: docker builder prune failed: {tail(err)}")
    else:
        result["reclaimed"] = reclaimed(out)
    return result
