# Образ входа brood (brood deploy server): nginx с TLS, маршрутами и собранной
# панелью (brood/server/web). Контекст -- корень рабочей копии; что видно
# сборке, решает Dockerfile.dockerignore рядом.
#
# Bun -- той версии, что закреплена в package.json панели (packageManager); тот
# же образ берёт brood dev web.
FROM oven/bun:1.3.14-slim@sha256:d56a2534ffd262e92c12fd3249d3924d296d97086da773f821d7d0477435ea04 AS panel
WORKDIR /web
COPY brood/server/web/package.json brood/server/web/bun.lock ./
RUN bun install --frozen-lockfile
COPY brood/server/web/ ./
# Версия, которую панель показывает о себе: апстрим, от которого форк, и brood.
# Правится вместе с git subtree pull (docs/WEB.md).
RUN VERSION=v1.26.7+brood bun run build

FROM nginxinc/nginx-unprivileged:1.30-alpine@sha256:f5116ed4606f3eb015dd4a8e28cb9b351476dc0ad4bc6e9f93b19cec130cdf03
# Шаблон: entrypoint образа подставляет в него ${CLIPROXY_*} из окружения
# (NGINX_ENVSUBST_FILTER в манифесте) и кладёт в conf.d.
RUN rm -f /etc/nginx/conf.d/default.conf
COPY brood/dev/deploy/edge/nginx.conf.template /etc/nginx/templates/default.conf.template
COPY brood/dev/deploy/edge/django.inc brood/dev/deploy/edge/cliproxy.inc /etc/nginx/brood/
COPY --from=panel /web/dist/index.html /usr/share/nginx/html/index.html
