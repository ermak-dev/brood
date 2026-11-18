"""Проектные MR через настроенный транспорт GitLab; данные не доказывают состояние git."""
import http.client
import re

from brood.dev import gitlab


_SHA = re.compile(r"[0-9a-f]{40}")
_REFUSALS = {
    400: "missing or invalid SHA",
    401: "insufficient authentication or rights",
    403: "insufficient rights",
    404: "MR or project not found or not accessible",
    405: "MR is not eligible for merge",
    409: "source HEAD changed",
    422: "merge execution failed",
}


def _iid(value):
    text = str(value)
    if isinstance(value, bool) or len(text) > 19 or not re.fullmatch(r"[1-9][0-9]*", text):
        raise RuntimeError("invalid MR IID: expected a positive 64-bit integer")
    number = int(text)
    if number > 2**63 - 1:
        raise RuntimeError("invalid MR IID: expected a positive 64-bit integer")
    return number


def _sha(value):
    return isinstance(value, str) and _SHA.fullmatch(value) is not None


def _valid(data, iid):
    return (isinstance(data, dict) and type(data.get("iid")) is int
            and data["iid"] == iid and data.get("state") in ("opened", "closed", "locked", "merged")
            and _sha(data.get("sha"))
            and (data.get("merge_commit_sha") is None or _sha(data["merge_commit_sha"])))


def _call(method, path, payload=None):
    try:
        if payload is None:
            return gitlab.call(method, path)
        return gitlab.call(method, path, payload)
    except (RuntimeError, ValueError, OSError, http.client.HTTPException) as error:
        # Транспорт может включить чужое тело или сетевую причину с секретом.
        status = re.match(re.escape(f"{method} {path} -> HTTP ") + r"([0-9]{3})\b", str(error))
        code = int(status[1]) if status else None
        if code in _REFUSALS:
            raise RuntimeError(f"MR request refused: HTTP {code}: {_REFUSALS[code]}") from None
        if method == "PUT":
            raise RuntimeError("merge outcome unknown") from None
        raise RuntimeError("MR read failed: check GitLab configuration, credentials and connectivity") from None


def show(iid):
    """Только GET; неожиданный ответ не отражается в отказе."""
    iid = _iid(iid)
    data = _call("GET", f"/merge_requests/{iid}")
    if not _valid(data, iid):
        raise RuntimeError("invalid GitLab MR response")
    return data


def merge(iid, sha=None):
    """Один явный squash merge с CAS по прочитанному HEAD, без повторов."""
    iid = _iid(iid)
    if sha is not None and not _sha(sha):
        raise RuntimeError("invalid SHA: expected 40 lowercase hexadecimal characters")
    current = show(iid)
    if sha is not None and sha != current["sha"]:
        raise RuntimeError("source HEAD mismatch: expected SHA differs from current MR HEAD")
    if current["state"] == "merged":
        return current
    if current["state"] != "opened":
        raise RuntimeError(f"MR is {current['state']}: cannot merge")
    try:
        data = _call("PUT", f"/merge_requests/{iid}/merge",
                     {"squash": True, "auto_merge": False, "sha": current["sha"]})
    except RuntimeError as error:
        if str(error) != "merge outcome unknown":
            raise
        data = None
    if not _valid(data, iid) or data["sha"] != current["sha"]:
        raise RuntimeError(f"merge outcome unknown; inspect brood dev mr show {iid} "
                           "in the same project before any retry; no automatic retry") from None
    return data
