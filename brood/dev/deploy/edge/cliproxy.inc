# Запрос к CLIProxyAPI (nginx.conf.template): URI как есть, WebSocket для
# /v1/realtime, без буферизации -- ответы LLM идут стримом и долго.
proxy_pass http://cliproxy;
proxy_http_version 1.1;
proxy_set_header Host $http_host;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection $connection_upgrade;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto https;
# Сессия brood прокси ни к чему.
proxy_set_header Cookie "";
proxy_buffering off;
proxy_request_buffering off;
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;
