# Deploy

Зелёный main выкладывает Go-сервер `brood` и исходную React-панель. Образ
`deploy/Dockerfile.go` содержит только два статических бинарника `brood`/`bro`,
CA bundle и приватный `/data`. Bun используется только при сборке панели;
Python, shell и frontend toolchain в server image нет.

## Состояние и данные

По решению оператора от 2026-10-27 старые Django users/sessions не переносятся.
Создаётся новая SQLite-база с текущей схемой и новый администратор. Legacy
import CLI и versioned upgrade paths удалены: несовместимую БД нужно явно
пересоздать. Обычная повторная выкладка сохраняет текущую Go-БД и пароль
существующего администратора, не сбрасывает их автоматически.

Сброс серверного приложения не означает пересоздание общего Substrate.
Namespace `brood`, WorkerPools, акторы, их volumes и registry сохраняются.
Серверная выкладка не вызывает Substrate install/start/uninstall и не меняет
runtime/kernel. Пользовательские coding-акторы не используются для проверки.

## Команды

```sh
bin/brood deploy server --check
bin/brood deploy server
```

Preflight проверяет настройки, TLS, владение установкой/namespace, approved
worker images, точные Kubernetes API endpoints и identities существующих
server/edge ресурсов до сборки и записи. Namespace должен уже принадлежать
установке; новый Substrate подготавливается отдельной операцией.

Полная выкладка:

1. Собирает pinned TLS edge и Go-образ, получает digest именно своего build.
2. Применяет приватный Go Deployment/Service/PVC/Secrets/NetworkPolicy и
   ждёт готовности; SQLite инициализируется самим сервером.
3. Выполняет `/brood bootstrap-admin` внутри доверенного server pod, передавая
   username/password через stdin, не argv и не CI stdout. Это не actor pod.
4. Обновляет TLS edge с сохранением existing Service identity, clusterIP и
   NodePorts `30080`/`30443`; трафик идёт к Go по HTTPS с проверкой сертификата.
5. Проверяет public HTTPS, React, login администратора, его actual admin flag
   и management backend (если настроен), затем logout. Проверка входа обязательна
   также при retry — существующий admin не отменяет gate.
6. Только после успешной проверки удаляет конкретные старые Django/Postgres
   deployments/services/policies и `data-postgres-0`, сверяя UID с preflight.
   Ни namespace целиком, ни worker/actor ресурсы не входят в cleanup allowlist.

`bin/brood deploy go` остаётся подготовкой только приватного Go endpoint,
без public edge, bootstrap пользователя или очистки старых данных.

## Настройки

Секреты задаются только приватным `.env` релиза, который передаёт CI.

| Настройка | Назначение |
|---|---|
| `BROOD_SESSION_SECRET` | случайный ключ, минимум 32 символа |
| `BROOD_PUBLIC_HOST` | DNS-имя публичного входа, присутствующее в TLS certificate |
| `BROOD_HTTPS_PORT`, `BROOD_HTTP_PORT` | опубликованные host ports, задаются при создании kind |
| `BROOD_ADMIN_USERNAME`, `BROOD_ADMIN_PASSWORD` | initial admin и проверка доступа; пароль минимум 12 символов |
| `WORKER_POOL_IMAGES` | JSON approved digest images: обязательный gvisor, необязательный microvm |
| `CLIPROXY_URL`, `CLIPROXY_MANAGEMENT_KEY` | опциональный существующий backend `http://cliproxy:8317` и его server-only key |

TLS: `secrets/tls.crt` и `secrets/tls.key`. Go и edge используют один
сертификат; upstream SNI `brood` требует соответствующего SAN. Проверка
public endpoint использует точный certificate и loopback resolve, не
`curl -k` и не случайный DNS destination.

После смены admin-пароля через UI нужно синхронно обновить deployment check
credential в protected `.env`, иначе следующая auth-проверка закономерно
откажет. Bootstrap не меняет пароль уже существующей учётной записи.
Пароли, session cookies и provider config не печатаются в CI-логи.

Историческое имя ServiceAccount `django` — точный доверенный принципал
Substrate/atenet-router, не Python runtime. Его и worker-pool RBAC не
удаляют с legacy сервером. Project membership и доступ к своим роям проверяет
Go BFF до использования этой привилегированной service identity.

CLIProxyAPI/provider credentials живут отдельно и не являются Django user
migration. Для Go разрешается только scoped network access к этому backend;
management key хранится в server Secret и не попадает в browser/edge.

Для `bro push` deploy после проверки владения Docker container/ID обнаруживает
IP внутреннего registry в kind network. Go получает `BROOD_REGISTRY_URL` и
`BROOD_REGISTRY_IMAGE_HOST=localhost:5002`, а NetworkPolicy — egress только на
найденный IP:5000. Registry не перепубликуется наружу: внешний клиент передаёт
chunks до 32 MiB через обычный HTTPS session/CSRF API. Ограничение edge 64 MiB
сохраняется. Подробности — `docs/BUILD.md`.

## CI и restricted SSH

Используется прежний runner builder с тегом docker; runner на target не
добавляется. `.ci/test.sh` проверяет Python dev tools, `.ci/go-test.sh` —
React, Go и cross-release amd64/arm64. Только успешный default branch
переходит в deploy stage.

`.ci/deploy.sh` посылает git archive и полный SHA по SSH. В архив добавлены
приватные `.env` и TLS из protected CI file variables с правами 0600:

| Переменная | Назначение |
|---|---|
| `BROOD_DEPLOY_HOST` | SSH destination установки |
| `BROOD_DEPLOY_SSH_KEY` | ограниченный deploy key |
| `BROOD_DEPLOY_KNOWN_HOSTS` | проверенный host key |
| `BROOD_DEPLOY_ENV` | `.env` релиза |
| `BROOD_DEPLOY_TLS_CERT`, `BROOD_DEPLOY_TLS_KEY` | TLS |

Ключ в authorized_keys имеет restrict/from/forced command `brood-deploy`.
Команда принимает только SHA и архив, serializes через flock, проверяет
release до мутаций и запускает только application rollout. `BROOD_RELEASE_SHA`
передаётся Docker build для version stamp. Копия `/usr/local/bin/brood-deploy`
обновляется штатным provision/install отдельно от release; обычный deploy
не должен повторно раскатывать работающие atelet/worker components.

Настройки обновляет `bin/brood dev ci set --file VAR=PATH`, имена/флаги без
значений показывает `bin/brood dev ci vars`. В репозитории нет target address,
private keys или admin password. Private provisioning инструкции и
`deploy/provision.sh` сохраняют прежний restricted SSH interface.

## microVM и workbench

Изолированная developer-нагрузка использует отдельную microVM с собственными
Docker/kind и свежими секретами (`docs/WORKBENCH.md`). Отсутствие ёмкости —
блокер, а не разрешение остановить пользовательский актор или менять host
runtime policy. Server deploy никогда не вызывает перевод пулов на microVM.
