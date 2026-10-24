"""Вход панели и ворота management API прокси.

Панель (brood/server/web) входит в сессию Django, а не в CLIProxyAPI: ключ
management в браузер не попадает. Перед каждым запросом панели в
/v8/management nginx входа спрашивает /auth/llm (auth_request) и, получив
2xx, сам ставит ключ (brood/dev/deploy/edge/nginx.conf.template).
"""
import json

from django.contrib import auth
from django.http import HttpResponse, JsonResponse
from django.views.decorators.csrf import ensure_csrf_cookie
from django.views.decorators.http import require_GET, require_POST

SAFE_METHODS = ("GET", "HEAD", "OPTIONS")


def state(request):
    user = request.user
    return {"authenticated": user.is_authenticated,
            "username": user.get_username() if user.is_authenticated else "",
            "is_superuser": user.is_authenticated and user.is_superuser}


@require_GET
@ensure_csrf_cookie
def session(request):
    """Кто вошёл; заодно ставит куку csrftoken, которую вернут login и logout."""
    return JsonResponse(state(request))


@require_POST
def login(request):
    try:
        body = json.loads(request.body or b"{}")
    except ValueError:
        return JsonResponse({"error": "the body must be JSON"}, status=400)
    user = auth.authenticate(request, username=str(body.get("username", "")),
                             password=str(body.get("password", "")))
    if user is None:
        return JsonResponse({"error": "invalid username or password"}, status=401)
    auth.login(request, user)
    return JsonResponse(state(request))


@require_POST
def logout(request):
    auth.logout(request)
    return JsonResponse(state(request))


def same_origin(request):
    """Origin запроса панели -- этот же сайт. Чистая проверка: сессия едет
    кукой, и чужая страница могла бы дёрнуть management от имени вошедшего.
    Браузер шлёт Origin с каждым небезопасным запросом fetch."""
    origin = request.headers.get("Origin", "")
    return origin == f"{request.scheme}://{request.get_host()}"


def llm(request):
    """Ворота /v8/management для auth_request: 204 -- пускать, 401 -- нет
    сессии, 403 -- не суперпользователь или чужой Origin. Метод исходного
    запроса nginx передаёт заголовком: подзапрос сам по себе -- GET."""
    if not request.user.is_authenticated:
        return HttpResponse(status=401)
    if not request.user.is_superuser:
        return HttpResponse(status=403)
    method = request.headers.get("X-Original-Method", "GET").upper()
    if method not in SAFE_METHODS and not same_origin(request):
        return HttpResponse(status=403)
    return HttpResponse(status=204)
