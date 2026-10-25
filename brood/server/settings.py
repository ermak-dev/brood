"""Настройки Django сервера brood.

Секреты и переключатели -- из .env рабочей копии (brood.common.config), а не
литералами: SECRET_KEY в коде уехал бы в git. Для разработчика их заводит
`brood dev setup`.
"""
import os

from django.core.exceptions import ImproperlyConfigured

from brood.common import config

SECRET_KEY = config.get("DJANGO_SECRET_KEY")
if not SECRET_KEY:
    raise ImproperlyConfigured("DJANGO_SECRET_KEY is not set in .env: run brood dev setup")
# Опечатка в .env не выключает и не включает отладку молча.
DEBUG = {"1": True, "0": False, "": False}.get(config.get("DJANGO_DEBUG"))
if DEBUG is None:
    raise ImproperlyConfigured("DJANGO_DEBUG in .env: 1 or 0")
# Имена, под которыми сервер отвечает: через запятую, `brood,brood.corp.ermak.dev`.
# Пусто -- только localhost отладки.
ALLOWED_HOSTS = [h.strip() for h in config.get("DJANGO_ALLOWED_HOSTS").split(",") if h.strip()]
# TLS снимает nginx перед сервером (brood/dev/deploy/manifests/nginx.conf) и
# сам ставит этот заголовок. Без него Django считал бы запрос http, и вход в
# админку падал бы на проверке Origin.
SECURE_PROXY_SSL_HEADER = ("HTTP_X_FORWARDED_PROTO", "https")
SESSION_COOKIE_SECURE = CSRF_COOKIE_SECURE = not DEBUG

INSTALLED_APPS = [
    "django.contrib.admin",
    "django.contrib.auth",
    "django.contrib.contenttypes",
    "django.contrib.sessions",
    "django.contrib.messages",
    "django.contrib.staticfiles",
    "rest_framework",
    "brood.server.base",
    "brood.server.projects",
]

MIDDLEWARE = [
    # Первой: проба готовности отвечает до проверки Host, которой у kubelet нет.
    "brood.server.base.middleware.healthz",
    "django.middleware.security.SecurityMiddleware",
    "django.contrib.sessions.middleware.SessionMiddleware",
    "django.middleware.common.CommonMiddleware",
    "django.middleware.csrf.CsrfViewMiddleware",
    "django.contrib.auth.middleware.AuthenticationMiddleware",
    "django.contrib.messages.middleware.MessageMiddleware",
    "django.middleware.clickjacking.XFrameOptionsMiddleware",
]

ROOT_URLCONF = "brood.server.urls"

TEMPLATES = [
    {
        "BACKEND": "django.template.backends.django.DjangoTemplates",
        "DIRS": [],
        "APP_DIRS": True,
        "OPTIONS": {
            "context_processors": [
                "django.template.context_processors.request",
                "django.contrib.auth.context_processors.auth",
                "django.contrib.messages.context_processors.messages",
            ],
        },
    },
]

WSGI_APPLICATION = "brood.server.wsgi.application"

# Postgres -- когда задан POSTGRES_HOST: так сервер живёт в кластере (brood
# deploy server). Без него -- SQLite рабочей копии, на нём runserver
# разработчика и brood server test.
POSTGRES_HOST = config.get("POSTGRES_HOST")
if POSTGRES_HOST:
    POSTGRES = {name: config.get(f"POSTGRES_{name}") for name in ("DB", "USER", "PASSWORD")}
    missing = [f"POSTGRES_{name}" for name, value in POSTGRES.items() if not value]
    if missing:
        raise ImproperlyConfigured(f"POSTGRES_HOST is set, but not {', '.join(missing)}")
    DATABASES = {
        "default": {
            "ENGINE": "django.db.backends.postgresql",
            "HOST": POSTGRES_HOST,
            "PORT": config.get("POSTGRES_PORT") or "5432",
            "NAME": POSTGRES["DB"],
            "USER": POSTGRES["USER"],
            "PASSWORD": POSTGRES["PASSWORD"],
        }
    }
else:
    DATABASES = {
        "default": {
            "ENGINE": "django.db.backends.sqlite3",
            "NAME": os.path.join(config.PROJECT, "db.sqlite3"),
        }
    }

AUTH_PASSWORD_VALIDATORS = [
    {"NAME": "django.contrib.auth.password_validation.UserAttributeSimilarityValidator"},
    {"NAME": "django.contrib.auth.password_validation.MinimumLengthValidator"},
    {"NAME": "django.contrib.auth.password_validation.CommonPasswordValidator"},
    {"NAME": "django.contrib.auth.password_validation.NumericPasswordValidator"},
]

LANGUAGE_CODE = "en-us"
TIME_ZONE = "UTC"
USE_I18N = True
USE_TZ = True

STATIC_URL = "static/"
# collectstatic собирает сюда статику при сборке образа; без отладки её отдаёт
# whitenoise, в отладке -- runserver.
STATIC_ROOT = os.path.join(config.PROJECT, "static")
if not DEBUG:
    MIDDLEWARE.insert(MIDDLEWARE.index("django.middleware.security.SecurityMiddleware") + 1,
                      "whitenoise.middleware.WhiteNoiseMiddleware")

DEFAULT_AUTO_FIELD = "django.db.models.BigAutoField"

# ─── ateapi Substrate ─────────────────────────────────────────────────────
# Клиент проектов (brood/server/projects). В под -- projected ServiceAccount
# токен с audience api.ate-system.svc и ClusterTrustBundle -- CA serving
# сертификата ateapi (brood/dev/deploy/manifests/django.yaml). Свойства
# машины установки -- настройки .env, не дефолты кода; без них проекта нет,
# и /api/projects отвечает 503 с причиной.
ATEAPI_TOKEN = config.get("ATEAPI_TOKEN")
ATEAPI_CA = config.get("ATEAPI_CA")
# Пусто -- сервис ateapi внутри кластера. Разработчик с сервером вне кластера
# ставит адрес своего port-forward (kubectl port-forward -n ate-system svc/api).
ATEAPI_ENDPOINT = config.get("ATEAPI_ENDPOINT")

REST_FRAMEWORK = {
    "DEFAULT_AUTHENTICATION_CLASSES": ["rest_framework.authentication.SessionAuthentication"],
    "DEFAULT_PERMISSION_CLASSES": ["rest_framework.permissions.IsAdminUser"],
}

TEST_RUNNER = "brood.server.runner.Runner"
