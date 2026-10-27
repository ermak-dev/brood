# Контекст сборки — корень рабочей копии; вход не собирает панель.
FROM nginxinc/nginx-unprivileged:1.30-alpine@sha256:f5116ed4606f3eb015dd4a8e28cb9b351476dc0ad4bc6e9f93b19cec130cdf03
COPY brood/dev/deploy/edge/nginx.conf /etc/nginx/nginx.conf
USER 65532:65532
# Штатный entrypoint правит конфигурацию, что несовместимо с read-only root.
ENTRYPOINT ["nginx"]
CMD ["-g", "daemon off;"]
