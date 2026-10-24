# Запрос к Django (nginx.conf.template). Host -- как прислал клиент, с портом:
# по нему Django сверяет ALLOWED_HOSTS и Origin входа. X-Forwarded-Proto ставим
# сами (SECURE_PROXY_SSL_HEADER), клиентский затирается.
proxy_pass http://django;
proxy_set_header Host $http_host;
proxy_set_header X-Forwarded-Proto https;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
