# Образ сервера brood (brood deploy server): дистрибутив server каталогами --
# common и server -- и bin/brood, через который запускается всё. Контекст --
# корень рабочей копии; что из него видно сборке, решает
# Dockerfile.dockerignore рядом: .env и secrets/ в образ не попадают.
FROM python:3.12-slim@sha256:a0f57064cbdcd6285fa45f51237ab0b35ea8ebaed06a1f5973e89e19dfb86704

ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /app

# Зависимости -- extra server из pyproject.toml, отдельным слоем: правка кода
# их не пересобирает. brood dev setup здесь нет: модуля dev в образе нет.
COPY pyproject.toml .
RUN python -c 'import tomllib; print("\n".join(tomllib.load(open("pyproject.toml", "rb"))["project"]["optional-dependencies"]["server"]))' >/tmp/requirements.txt \
    && python -m venv venv \
    && venv/bin/pip install --no-cache-dir -r /tmp/requirements.txt \
    && rm /tmp/requirements.txt

COPY bin/brood bin/brood
COPY brood/__init__.py brood/
COPY brood/common brood/common
COPY brood/server brood/server

# Статика админки -- в образ, её отдаёт whitenoise. Ключ здесь -- заглушка
# сборки: настоящий приходит в под из Secret.
RUN DJANGO_SECRET_KEY=collectstatic DJANGO_DEBUG=0 bin/brood server collectstatic --noinput

USER 65532:65532
ENTRYPOINT ["bin/brood", "server"]
CMD ["serve", "--bind", "127.0.0.1:8000"]
