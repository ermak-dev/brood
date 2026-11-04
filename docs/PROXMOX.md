# Пулы Proxmox

## Два разных пути исполнения

Класс `lxc` у обычного Substrate WorkerPool запускает LXC внутри worker pod.
Такой актор не является CT, управляемой Proxmox. Эти пулы и полные профили
`development`/`coding` сохраняются без смены runtime при выкладке сервера.

Управляемый пул Proxmox использует другой путь:

```text
Brood Go API + SQLite
  -> проверенный HTTPS API Proxmox
     -> отдельная unprivileged CT для каждого актора
        -> доверенный root supervisor
           -> Go PTY holder + Claude Code/ttyd от UID/GID 1000
```

В этом пути нет фиктивного Kubernetes worker или `workerAssignment`.
Пул показывает собственное резервирование, а актор — provider, node, CTID,
поколение и фазу. Рой сохраняет свою настоящую привязку к Substrate atespace;
права проекта и роя проверяются до обращения к provider.

## Ограниченный профиль

Первый профиль называется `claude-proxmox`:

- отдельная непривилегированная CT;
- инструментарий разработки, ttyd и bootstrap-бинарь brood-guest из
  закреплённого OCI-образа; claude в образ не входит — bootstrap скачивает
  brood-guest/bro/claude с сервера при старте CT (настройка
  `BROOD_ARTIFACTS_DIR`, #155);
- постоянные `/workspace/home` и `/workspace/project` внутри диска CT;
- терминал через аутентифицированный обратный relay;
- root-owned DNS/HTTP proxy и nft policy, не управляемые процессом актора;
- UID/GID 1000, `NoNewPrivs=1`, без effective/ambient capabilities;
- без Docker/kind, доступа к host kernel log и memory checkpoint.

Новый workspace пуст, пока у актора нет origin; с origin первый старт
приложения клонирует его (см. «Origin и первый клон»). Секреты оператора,
SSH-конфигурация, `.env`, сессия Brood и credentials модели в образ или
workspace не копируются. Запуск
`claude --version` или открытие CLI не является проверкой платного model task.
В новом guest image Claude использует локальный шлюз CLIProxyAPI установки:
`ANTHROPIC_BASE_URL=http://127.0.0.1:8317`, discovery моделей включён.
`ANTHROPIC_AUTH_TOKEN=brood-local` — несекретная локальная заглушка для CLI,
не ключ модели и не полномочие сервера. Root gateway подставляет scoped relay
токен только на TLS-запросе к своему серверу, сервер — отдельный API key
CLIProxyAPI. При отсутствии серверной настройки запрос получает явный отказ.

Остановка и повторный запуск той же CT сохраняют workspace. **Reset создаёт
новую CT и безвозвратно удаляет прежний workspace.** Интерфейс требует явного
подтверждения. Удаление актора также удаляет его диск. Suspend с memory
checkpoint не поддерживается и отклоняется до внешних действий.

## Приватная конфигурация установки

`BROOD_PROXMOX_POOLS` и `BROOD_PROXMOX_TOKENS` — JSON-объекты в `.env`.
Координаты PVE, bridge, storage и ссылка на credential задаются явно: дефолтов
машины разработчика нет. Секрет токена находится только во втором объекте;
API панели возвращает разрешённые публичные поля подключения, а не credential
reference, CA или значение секрета.

Исходные поля подключения: `endpoint`, `tokenID`, `node`, `resourcePool`,
`storage`, `network`, `credentialRef`, `caPEM`. Установка с одним лишь
подключением остаётся read-only: просмотр не создаёт пул и не обращается к PVE.

Для исполнения добавляются все три поля одновременно:

```json
{
  "template": "templates:vztmpl/brood-claude-proxmox-<archive-sha256>.tar",
  "templateDigest": "sha256:<archive-sha256>",
  "capacity": {
    "actors": 2,
    "cpuMillicores": "4000",
    "memoryBytes": "4294967296",
    "diskGiB": 32
  }
}
```

Это **суммарные** ограничения пула. Пример даёт каждому из двух акторов
2 CPU, 2 GiB памяти и 16 GiB диска; профиль делит ёмкость на равные доли с
округлением вниз до целых CPU, MiB и GiB. Неподдерживаемые размеры отклоняются.
Провайдер не считает свободные ресурсы всего PVE-хоста разрешением превысить
эти ограничения.

Также требуются:

- `BROOD_PUBLIC_ORIGIN`: явный HTTPS origin сервера, доступный из CT и
  соответствующий TLS-сертификату;
- `BROOD_PROXMOX_RELAY_SECRET`: отдельный случайный ключ не короче 32 байт,
  не равный session secret. Он остаётся на сервере.

Настройки доставляются только в Secret и env сервера. Изменение Secret
меняет checksum pod template и вызывает обычный rollout даже при том же
образе. Egress сервера к PVE ограничивается разрешёнными IP подключения и
точным TCP-портом; отказ DNS прекращает выкладку, а не открывает широкую сеть.
Устройство выкладки и защищённые переменные описаны в [DEPLOY.md](DEPLOY.md).
Ротация relay key отзывает прежние guest tokens и меняет SSH host key:
текущим CT нужен контролируемый stop/bootstrap/start. Одна лишь выкладка
сервера не переписывает окружение уже работающего гостя.

Пул активируется администратором явно. Состояние «настроено» означает наличие
конфигурации, а не успешную проверку гостя или готовность актора.

## Подключение из панели

Администратор может создать подключение через `POST /api/proxmox-connections`:
`name`, `endpoint`, `tokenID`, `tokenSecret`, `caPEM`, `node`, `resourcePool`,
`storage`, `network`. Endpoint и CA вводятся явно, PVE token не передаётся
гостю. Поля `template`, `templateDigest`, `capacity` необязательны, но для
runtime задаются вместе в форме, точно в формате выше. Без них подключение
видно в инвентаре, но активировать его нельзя. Runtime-имя — lowercase DNS
label не длиннее 48 символов. Ввод проверяется теми же правилами, что env;
неизвестные поля, повторы JSON-ключей и дополнительные JSON-документы
отклоняются.

SQLite хранит подключение, CA, write-only token secret и одобренную runtime
конфигурацию. API не возвращает token secret или CA; строковые и JSON
представления входной структуры также их скрывают. База и резервные копии
содержат credential в открытом виде и требуют такого же ограничения доступа,
как Secret установки. Миграция v5 -> v6 добавляет runtime без сброса строк.
Изменение и ротация подключений через API пока не поддерживаются.

Для независимой приёмки нового guest image администратор может вызвать
`POST /api/proxmox-connections/{uid}/clone`, где `uid` — UID подключения
из БД. Тело содержит только обязательные `name`, `template`, `templateDigest`
и `capacity` в формате выше. Неизвестные поля, регистровые варианты имён,
повторы ключей, `null`, неверные типы и выход за ограничения runtime отклоняются.
Endpoint, tokenID, token secret, CA, node, resourcePool, storage и network
наследуются сервером из текущей записи источника; переопределить их нельзя.
Клонирование env-подключений не поддерживается.

Запрос требует действующую сессию администратора, same-origin `Origin` и
CSRF token. Перед записью сервер повторно проверяет сессию и текущую роль.
Ответ `201` — обычный metadata DTO подключения, без token secret и CA.
Ошибки не возвращают credential или исходный payload: `400` — неверная
структура, `422` — отсутствующие или невалидные поля, `404` — нет DB UID,
`409` — имя занято в БД или env. Создаётся только новое подключение:
сетевых запросов к PVE нет, источник, его пул и акторы не меняются.
Активация и удаление нового подключения/пула выполняются обычными API;
это не обновление immutable snapshot существующего пула (#109).

Активация выполняется отдельно:
`POST /api/proxmox-pools/{name}/activate` с пустым телом. Она сохраняет
снимок пула, но не проверяет PVE сетевым запросом. Дальнейшие операции акторов
используют сохранённые координаты и credential; перезапуск сервера не требует
переносить их в `BROOD_PROXMOX_POOLS` или `BROOD_PROXMOX_TOKENS`. При этом
`BROOD_PUBLIC_ORIGIN` и `BROOD_PROXMOX_RELAY_SECRET` по-прежнему обязательны
для runtime, даже когда env-пулы вообще не заданы.

Имена подключений env и БД не могут совпадать: создание из формы и запуск
сервера при такой коллизии отказывают. `DELETE /api/proxmox-connections/{uid}`
отклоняется, пока у подключения есть неудалённый managed pool или актор.
Сначала требуется подтверждённая очистка акторов и отдельное удаление пула;
исторические tombstone не препятствуют удалению подключения.

Сохранение формы не меняет сетевую политику кластера: доступ сервера к новому
PVE endpoint должен быть разрешён оператором отдельно. Форма не загружает
шаблон, не выдаёт токену права и не проверяет доступность реального PVE.

## Сборка и публикация образа

`.ci/proxmox-guest.sh OUTPUT_DIRECTORY` собирает Linux/amd64 OCI `.tar` из
чистого committed HEAD. Контекст компиляции и финального образа разрешён
явным списком; секреты и ignored-файлы рабочей копии не берутся. Скрипт
проверяет закреплённые версии и checksum инструментов и выдаёт
`proxmox-guest.tar` плюс `metadata.json` с commit и SHA-256 архива.

claude в образ не входит (#155): образ содержит тулчейн, ttyd и
bootstrap-бинарь brood-guest; runtime копии brood-guest, bro и claude
bootstrap скачивает с сервера при старте CT из каталога настройки
`BROOD_ARTIFACTS_DIR` после поднятия сети (адрес приходит по DHCP) и до
секретов и сервисов актора, с проверкой sha256 и ELF каждого артефакта.
Без настройки или артефактов новый образ отказывает fail closed; точное
имя настройки называет отказ сервера при неверном значении. CT из прежних
шаблонов маршрут не вызывают и работают со встроенными бинарями как
раньше. Скачанные бинари живут в `/opt/brood/bin` и оттуда запускаются
Claude и MCP-сервер `bro`.

Непустой output directory и перезапись результата запрещены. Сборка требует
Docker buildx; выбранный `BUILDX_BUILDER` используется и для компиляции, и для
финального образа. Опциональный `--seed` добавляет очищенный снимок проекта
как bundle в `/opt/seeds`; supervisor автоматически его не устанавливает.

Архив публикуется в template storage под неизменяемым именем с digest. Перед
активацией оператор проверяет соответствие содержимого, имени и настроенного
`templateDigest`. Runtime API token не получает права загружать шаблоны.
В установленном PVE должна поддерживаться распаковка OCI `.tar`; наличие
обычной LXC template storage само по себе этого не доказывает.

OCI import в PVE заменяет env значениями образа. Поэтому provider после
создания выполняет отдельный `ConfigureBootstrap` остановленной CT: проверяет
владение и digest конфигурации и меняет только bootstrap env через digest CAS.
Перед каждым новым поколением токен и параметры relay задаются заново.

## Браузерный терминал driftty

Новый guest image содержит только браузерный клиент driftty v3.0.0
(`mdp/driftty`, commit `a4b8b2b38ee29b8fa5b979e70d37e1565b09cb0e`).
`brood/dev/pins.json` закрепляет commit, SHA-256 исходного архива,
SHA-256 готового `index.html` и digest образа Node для сборки. Docker build
проверяет архив до распаковки, ставит зависимости через upstream
`package-lock.json` с `npm ci --ignore-scripts`, затем собирает UI без сети
от непривилегированного пользователя. Итоговый образ получает только
`dist/index.html` и MIT license в `/usr/local/share/brood/driftty/`;
Node и gateway driftty из build stage в него не копируются.
После проверки upstream HTML добавляется локальный `driftty-storage.js`:
actor-view сохраняет `sandbox allow-scripts` с opaque origin, поэтому
недоступные localStorage/sessionStorage заменяются памятью текущей вкладки.
Ослаблять sandbox ради хранения настроек терминала не требуется. Тест
совместимости исполняется в build stage до публикации HTML.

Гостевой ttyd использует `--index /usr/local/share/brood/driftty/index.html`
на прежнем `127.0.0.1:8080`, с `--writable` и `--check-origin`. Клиент
обращается к прежним `/token` и `/ws` относительно пути терминала.
Новых публичных listeners, credentials, auth routes и разрешений relay нет.
Go PTY holder (#162) держит процесс Claude независимо от браузера. Ttyd
запускает только фиксированный `brood-guest --brood-guest-attach`; исходные
байты PTY доходят до driftty без промежуточного терминального эмулятора.
Закрытие вкладки отсоединяет клиента, не останавливая Claude. Headless VT
наблюдает поток только для снимков MCP и восстановления состояния экрана
при reconnect; его ответы терминалу не отправляются приложению.
Root создаёт `/run/brood/pty.sock` (root:1000, 0660) в защищённом каталоге и
передаёт listening FD 3 фиксированному UID 1000 процессу. Модель не выбирает
путь, команду или окружение holder. Writer и источник размера один:
второй browser writer, конфликтующие MCP input/resize и workspace-операции
с остановкой сессии отвергаются; read-only наблюдатели не меняют размер PTY.
Явные lifecycle stop/reset/delete CT сохраняют прежнюю семантику. Broker и внешние MCP/API
контракты сохранены; tmux больше не участвует в новом guest runtime.
При выходе приложения holder завершает оставшихся участников его группы
процессов до освобождения PID лидера. Если подтвердить очистку нельзя, он
возвращает `unknown`, удерживает идентичность и запрещает новый сеанс;
клиент и supervisor получают причину, а не успешное завершение.
Если bootstrap обновил бинарь старого образа без index, он сохраняет штатный
клиент ttyd; одна выкладка серверного бинаря не добавляет driftty в старые CT.

Выкладка сервера не заменяет уже работающий гостевой runtime. Переход
существующего актора с tmux требует согласованного перезапуска его сессии:
переноса живого Claude из PTY tmux в другой PTY нет. Простого stop/start
также недостаточно, если `/opt/brood/bin` содержит отличающиеся артефакты:
bootstrap отказывает в их перезаписи. Нужна отдельная контролируемая
процедура обновления гостевых бинарей с сохранением home и workspace,
не reset или удаление пользовательского актора.

Для приёмки после интеграции нужен новый committed guest image:

```sh
BUILDX_BUILDER=<builder> .ci/proxmox-guest.sh /tmp/brood-160-image
```

Каталог результата должен отсутствовать или быть пустым. Оператор публикует
полученный OCI-архив под именем с его SHA-256, сверяет `metadata.json` и
создаёт отдельное fixture-подключение через clone API с новыми `template` и
`templateDigest` (см. выше). Существующие подключения, пулы и пользовательские
акторы не обновляются и не reset-ятся ради этой проверки.

В отдельном fixture-пуле проверить новый актор через настоящий Go actor-view:
мобильную клавиатуру и resize, ввод/вывод команды с уникальным маркером,
разрыв и повторное открытие вкладки с сохранением PID/start/epoch и маркера,
обычные restart/recycle и MCP agents/send. Проверить HTTP index, `/token` и
WebSocket `/ws` под префиксом actor-view, отсутствие внешних JS/CSS/font
загрузок и нового gateway, прежние отказы без ticket/сессии, при чужом ACL и
после смены поколения. Проверка через прямой порт ttyd не заменяет эту
приёмку CSP и прокси. Удалять можно только созданные fixture-ресурсы после
отдельного подтверждения оператора, не чужие акторы.

## Терминал и сеть

Гость сам открывает WSS на
`/api/proxmox/guest/<actorUID>/<generation>`. Внутри соединения идёт SSH с
закреплённым публичным host key. Терминальный канал ведёт только к
`127.0.0.1:8080`; отдельный `brood-swarm-msg` передаёт ограниченные запросы
сообщений роя. Произвольные exec, SCP и port forwarding не предоставляются.
Гость получает HMAC-токен своего UID/поколения, но не установочный relay key,
PVE token или browser session. Публичная TLS chain передаётся в base64.

Machine route имеет собственную аутентификацию вне cookie/CSRF middleware.
Браузер использует прежний actor-view сессией, ACL, CSP и ticket, дополнительно
привязанным к managed UID и поколению. Просмотр не запускает остановленный
managed actor неявно. Подключённый relay ещё не означает готовность:
нужна успешная HTTP-проверка настоящего ttyd после завершения PVE task.

До запуска актора root supervisor завершает DHCP и устанавливает nft policy,
ограниченный DNS и HTTP/CONNECT proxy. Прямой IPv4/IPv6 egress UID 1000 закрыт;
proxy разрешает только точные одобренные публичные имена и порты. Проверяются
все resolved addresses, затем соединение открывается к уже проверенному IP.
Private, loopback, link-local, multicast и unspecified targets не становятся
разрешёнными из-за DNS rebinding. Удаление proxy env актором не снимает nft.

Отказ enforcement не разрешает запуск с добровольным proxy-only режимом.
Повторный supervisor должен эксклюзивно создать nft table и не может принять
или удалить правила уже работающего supervisor. Для этого профиля не нужно
менять глобальные PVE firewall, AppArmor, sysctl или bridge policy.

## Автономный запуск Claude

Claude стартует без человека у терминала. При загрузке, после DHCP, секретов,
origin, gateway и сервисов актора, root supervisor запускает собственный
бинарник с фиксированным `--brood-guest-autostart` от UID/GID 1000 с
NoNewPrivs и фиксированным env. Helper выполняет idempotent `ensure` через
защищённый сокет Go PTY holder. Живая сессия не заменяется; новую запускает
holder фиксированной командой `brood-guest --brood-guest-app`, как restart.
Сам holder и ttyd остаются в `/workspace/home`; только app wrapper выбирает
`/workspace/project`. Browser attach не создаёт и не оживляет сессию после
её явной остановки. Отказ helper пишется одной строкой в stderr supervisor
и повторяется с паузой от 1 до 30 секунд до успеха или остановки; supervisor
от него не падает. Проектные секреты читает только app wrapper, а не holder
или browser attach.

Wrapper запускает `claude --dangerously-skip-permissions --mcp-config ...`,
как mop. Перед exec он сливает в `~/.claude.json`
`hasCompletedOnboarding=true`, `theme` (если не задан, `dark`),
`resumeReturnDismissed=true` и
`projects["/workspace/project"].hasTrustDialogAccepted=true`, а в
`~/.claude/settings.json` — `skipDangerousModePermissionPrompt=true`.
Прочие ключи сохраняются байт в байт. Запись атомарна (новый inode и rename),
symlink, hard link, чужой владелец и файл больше 1 MiB — отказ без
изменений; повреждённый файл или не-объект заменяется минимальным объектом
с этими ключами, но не пустым файлом. Неудача настройки — предупреждение в
терминале, Claude всё равно стартует.

Bypass снимает запросы разрешений внутри CT: модель выполняет инструменты
без подтверждения. Граница остаётся прежней — UID 1000 без capabilities,
nft, DNS и HTTP/CONNECT proxy root supervisor, machine credentials вне
актора. Не выдавайте проекту секреты, которые его код и модель не должны
использовать ([SECRETS.md](SECRETS.md)).

## Origin и первый клон

Bootstrap env остаётся совместимым с прежними образами: новых
`BROOD_GUEST_*` переменных origin нет. Новый root supervisor при загрузке
получает `{"origin":"...","mode":"gateway|direct"}` через
`GET /api/proxmox/workspace/{uid}/{generation}` с scoped guest token и
проверкой TLS. Пустой origin возвращается вместе с пустым mode. Путь,
поколение, активная привязка роя и namespace проверяются как при доставке
проектных секретов; пользовательская cookie не заменяет guest token.
Supervisor проверяет HTTPS origin, отсутствие credentials/query/fragment,
DNS-имя и лимит 1024 байта. Direct host обязан входить в
`BROOD_GUEST_ALLOWED_HOSTS`. Неверный ответ останавливает загрузку до актора.
Старый guest image нового endpoint не вызывает: он продолжает загружаться,
но не клонирует origin. Для clone нужен новый образ; наличие origin в панели
само по себе не подтверждает, что старый образ создал checkout.

Origin не секрет, но env приложения фиксирован и не расширяется из browser attach.
Поэтому root supervisor до запуска актора пишет пару в
`/run/brood/workspace-origin.json` (root:1000, 0440, рядом с проекцией
секретов), а UID 1000 wrapper `--brood-guest-app` читает его без symlink и с
той же проверкой. Каждый start CT пишет файл заново: сервер пересчитывает
режим при каждом start.

Клонирует wrapper при старте приложения, до файловых секретов, и только
если `/workspace/project` пуст. `gateway` — `git clone
http://127.0.0.1:8318/origin.git`, `direct` — `git clone <origin>` через
egress proxy. Remote `origin` клона указывает на тот URL, через который
клонировали, поэтому fetch и push идут тем же путём; канонический origin
записывается в `git config brood.origin`. Git запускается без системной и
пользовательской конфигурации, без credential helper, prompt и redirects,
только с протоколом `http` (gateway) или `https` (direct).

Git получает отдельный приватный staging-каталог в `~/.config/brood`, а не
живой workspace: при неудаче сам Git может рекурсивно удалить destination.
После успешного клона и записи `brood.origin` wrapper заново проверяет пустоту
workspace и переносит записи через `renameat2(RENAME_NOREPLACE)`, `.git` —
последней. Конкурентная запись не перезаписывается; частичная публикация
оставляет маркер и требует явного reset. Staging и workspace должны быть
на одной файловой системе, как в образе brood. Обычная ошибка удаляет только
созданный staging; после SIGKILL его остаток не публикуется и не даёт права
очищать workspace.

Ошибка клона — одна строка причины в терминале сессии; Claude всё равно
запускается. Следующий старт повторяет клон только при пустом workspace.
Непустой каталог не очищается автоматически даже после ошибки Git или при
маркере `~/.config/brood/workspace-clone.pending`: там могла появиться работа
между попытками или во время клонирования. После прерывания оператор явно
подтверждает reset, если требуется заново создать рабочую копию. Клоны
сериализуются flock; маркер не является разрешением на удаление данных.

### Git gateway

В режиме `gateway` root supervisor слушает `127.0.0.1:8318` (рядом с model
gateway на 8317; nft пропускает UID 1000 к этому порту только по loopback).
Принимаются только Host `127.0.0.1:8318` и пути
`/origin.git/info/refs` (GET, query ровно `service=git-upload-pack` или
`service=git-receive-pack`), `/origin.git/git-upload-pack` и
`/origin.git/git-receive-pack` (POST, без query). Dumb HTTP, другие
репозитории, percent-encoded пути, `Origin` header и тело у discovery —
отказ до сервера.

Запрос уходит на `https://<хост relay>/api/proxmox/git/<uid>/<generation>/<op>`
с scoped relay token в `Authorization: Bearer`. Authorization, cookies и
прочие заголовки актора отбрасываются; передаются только `Accept`,
`Content-Type`, `Content-Encoding`, `Git-Protocol`, обратно — `Content-Type`,
`Content-Encoding`, `Cache-Control`. Redirect — 502, 401/407 сервера —
403, чтобы Git актора не спрашивал пароль. Тела идут потоком в обе стороны
без буфера model gateway: upload-pack до 64 MiB, receive-pack до 1 GiB,
весь запрос до 30 минут, одновременно не более четырёх запросов. Учётные
данные remote остаются на сервере.

## Приём сообщений в госте

Гостевой relay обрабатывает входящий `brood-swarm-msg` отдельно от терминала:
до восьми доставок, не более 128 KiB на запрос и ответ, ожидание до 300 секунд.
Закрытие канала или SSH-сессии отменяет доставку. Root supervisor не читает
actor-owned session files и не открывает указанные в них сокеты. Он запускает
только собственный проверенный бинарник с фиксированным
`--brood-guest-message`, UID/GID 1000, без supplementary groups и bootstrap env.
Helper проверяет личность, сбрасывает capabilities и устанавливает NoNewPrivs
до чтения `~/.claude/sessions` и обращения к Unix-сокетам.

Получатель выбирается по единственной живой сессии `/workspace/project`.
Конверт заявляет `from-mode="bypass"`: managed Claude запускается с
`--dangerously-skip-permissions`, а режим конверта, отличный от режима
получателя, Claude Code удерживает как `mode-mismatch`.
`crossSessionInbound: accept` не устанавливается. Peer key передаётся первым кадром; обратный сокет создаётся
рядом с inbox, принимает квитанции только своего UID, ограничивает их размер
и сверяет `orig_msg_id`. `notify_when_idle` имеет отдельный идентификатор.

`delivered` возвращается только по явной совпавшей квитанции. `held`, `denied`,
`refused`, `expired`, `dropped` остаются отрицательными вердиктами. Запись кадра
без квитанции даёт `submitted; delivery unconfirmed`, даже если отдельно
подтверждён idle: ни доставка, ни попадание в очередь этим не доказаны.
Автоматический повтор после такого результата может создать дубликат.

Claude получает дополнительный MCP-сервер `brood` через `--mcp-config`, без
перезаписи пользовательских MCP-настроек. Инструменты `agents` и `send` используют
локальный root-owned broker; credentials остаются у supervisor. Сервер
выводит отправителя из текущей SSH identity и ограничивает список и доставку
managed-акторами того же роя. Native-акторы пока не адресуются этим каналом.
Протокол, ограничения и проверка описаны в [MCP.md](MCP.md).

## Серверный шлюз моделей

Managed-доступ к CLIProxyAPI требует `CLIPROXY_URL` и отдельного серверного
`CLIPROXY_API_KEY`. Это клиентский ключ прокси, не management key. Его значение
остаётся только у сервера; root supervisor гостя использует свой scoped relay
токен текущего UID/поколения. Для proxy с частной цепочкой TLS
`CLIPROXY_CA_PEM_B64` задаёт доверенные PEM-сертификаты в base64: доверие
проверяется явно, `InsecureSkipVerify` не используется. Для публичной цепочки
переменная не нужна. Эти корни применяются только к CLIProxyAPI backend.

Если акторы должны использовать другой proxy (например, существующий сервер
mop), задаются `BROOD_ACTOR_LLM_URL`, `BROOD_ACTOR_LLM_API_KEY` и, для частной
TLS-цепочки, `BROOD_ACTOR_LLM_CA_PEM_B64`. Origin обязан быть HTTPS, ключ —
явным и непустым. Эта настройка применяется только к managed-маршрутам:
`CLIPROXY_URL`, public `/v1/` и management-панель остаются на прежнем backend.
Без override используются общие настройки CLIProxyAPI выше. Accounts,
OAuth-файлы и список ключей чужой установки автоматически не переносятся.

Machine endpoints:

- `GET /api/proxmox/llm/<uid>/<generation>/v1/models`;
- `POST /api/proxmox/llm/<uid>/<generation>/v1/messages`;
- `POST /api/proxmox/llm/<uid>/<generation>/v1/messages/count_tokens`.

Для pinned Claude Code 2.1.310 оба gateway допускают только канонический
`limit=1..1000` у каталога и точный `beta=true` у messages/count_tokens.
Остальные, повторные и percent-encoded query-параметры отвергаются;
разрешённый query сохраняется до upstream. Проверка настоящим CLI в
контейнере без сети включает catalog discovery, synthetic SSE и раскрытие
`mcp__brood__agents`/`mcp__brood__send` через встроенный ToolSearch.
Модельные ответы этой проверки фиксированные, реальный provider не вызывается.

Сервер проверяет текущий start/running intent перед запросом, подставляет
серверный ключ и направляет запрос только в настроенный CLIProxyAPI origin.
Cookies, forwarded identity, management headers и guest Authorization upstream
не передаются. Отдавать `Set-Cookie`, следовать redirects или обращаться к
management paths через этот шлюз нельзя. Публичный `/v1/` сохраняет прежнюю
авторизацию клиентским ключом и не получает серверный ключ из этого маршрута.

Тело ограничено 8 MiB и 30 секундами чтения; ожидание response headers —
30 секунд, весь запрос — 30 минут. SSE передаётся без буферизации. Разрыв
клиентского соединения отменяет upstream; stop/reset отзывает новые запросы
сразу, уже открытый поток отменяется при проверке авторизации раз в секунду.
Наличие серверного шлюза само по себе не настраивает proxy внутри старого гостя.
Изменение guest image не должно уничтожать workspace работающего актора.

## Origin актора и серверный шлюз Git

Managed-актор хранит неизменяемый `origin` — HTTPS URL Git-репозитория,
который гость клонирует в пустой `/workspace/project`. Пустой origin — пустой
workspace, как раньше. Origin задаётся при создании: поле `origin` в
`POST /api/swarms/{id}/actors` (только managed; native-актор с этим полем
получает 400) или `add NAME --origin URL` у master. Без origin worker,
созданный master, наследует origin master; иначе берётся repository проекта;
иначе workspace пустой. JSON актора содержит `"origin"` и `"originMode"`:
`gateway`, `direct` или `""`.

Каноническая форма (пакет `gitorigin`): только `https://`, hostname DNS
(не IP и не числовая запись вроде `127.1`), порт отсутствует или 443,
непустой путь из сегментов `[A-Za-z0-9._~-]` без `.`/`..`, без userinfo,
query, fragment, percent-encoding и управляющих символов, не длиннее 1024
байт; host в нижнем регистре, без завершающего `/`. Хранится только
каноническая форма; отказ называет правило и не повторяет значение.

Режим вычисляется заново при чтении, на каждом start и на каждом запросе
шлюза:

- `gateway` — origin в точности равен repository проекта, и этот remote
  настроен в `BROOD_GIT_REMOTES`;
- `direct` — host origin точно входит в публичный allowlist гостя
  (`github.com`, `gitlab.com` и т.д.), клон идёт через egress proxy гостя;
- иначе создание отказывает 422 до резервирования, а не создаёт пустой
  workspace молча.

На каждом старте гостя supervisor заново получает origin и режим через
авторизованный workspace endpoint. Если origin потерял режим (repository
проекта сменился или remote убран), сервер возвращает оба поля пустыми:
гость не клонирует, шлюз отказывает. Bootstrap env не расширяется, учётные
данные remote в гостя не попадают. Схема SQLite 10 добавляет `managed_actors.origin` и таблицу
`project_repositories` аддитивно: прежние акторы получают пустой origin.

### Remotes установки и repository проекта

`BROOD_GIT_REMOTES` — секрет установки в `.env`: JSON-объект, ключ —
канонический HTTPS URL репозитория, значение `{"username":"...","token":"..."}`
или `{}` (без учётных данных). Неканонический ключ, неизвестное или
регистровое имя поля, повтор, только одно из двух полей, двоеточие в
username, непечатаемые символы или более 256 remotes — отказ запуска с
именем настройки, без значений. Токен остаётся на сервере: его нет в JSON,
логах, ошибках, окружении гостя и форматировании конфигурации.

- `GET /api/git-remotes` (администратор):
  `{"items":[{"origin":"...","credentialConfigured":true}]}`;
- `GET /api/projects/{id}/repository` (участники проекта): `{"origin":""}`;
- `PUT /api/projects/{id}/repository` (администратор, same-origin, CSRF):
  тело ровно `{"origin":"..."}`; значение канонизируется и должно быть
  настроенным remote, `""` очищает. Неизвестные и регистровые варианты полей,
  повторы, `null`, другие типы и лишние документы — 400; не remote — 422.
  Сессия и роль перепроверяются в транзакции записи.

### Шлюз

Гость не достаёт до частного Git установки: egress proxy гостя по-прежнему
отвергает private-адреса, и это не меняется. Сервер посредничает так же, как
шлюз моделей:

- `GET /api/proxmox/git/<uid>/<generation>/info/refs?service=git-upload-pack`
  или `?service=git-receive-pack` (query ровно такой);
- `POST /api/proxmox/git/<uid>/<generation>/git-upload-pack`;
- `POST /api/proxmox/git/<uid>/<generation>/git-receive-pack`.

Авторизация — ровно один `Authorization: Bearer <guest token>` той же проверки
UID, поколения, start intent, привязки роя и namespace, что у
`/api/proxmox/llm` и `/api/proxmox/secrets`; cookie панели не принимается.
Затем режим origin актора должен быть `gateway`. Upstream — `<origin>/<op>`
с Basic auth из remote. Вверх уходят только `Content-Type`, `Accept`,
`Content-Encoding`, `Git-Protocol` и фиксированный `User-Agent`; вниз —
`Content-Type`, `Content-Encoding`, `Cache-Control`. Cookies, `Set-Cookie`,
`WWW-Authenticate` и forwarded identity не проходят, redirect — 502, а не
переход. POST требует точный `application/x-git-*-request`. Тела потоковые:
upload-pack до 64 MiB, receive-pack до 1 GiB (413); весь запрос — 30 минут;
не более 4 одновременных запросов на актора (429). Stop, reset, смена
repository проекта или remote отменяют и открытый поток в течение секунды.
Тела и учётные данные не журналируются.

Модель доверия: **актор может читать и пушить с учётными данными установки
ровно в repository своего проекта**, и только пока он выбран и настроен.
Любой код или модель внутри актора получает это право, включая force-push и
удаление веток, если их разрешает credential. Выдавайте remote отдельный
токен с минимальными правами одного репозитория; защита веток на Git-сервере
остаётся границей. Другой remote установки или repository другого проекта
через шлюз недоступен: upstream определяется только origin актора.

Выкладка сервера должна отдельно доставить `BROOD_GIT_REMOTES` в Secret
`brood-go-env` и разрешить egress сервера к host remotes на 443; это не
входит в серверный код.

## Состояние, восстановление и освобождение ресурсов

SQLite хранит неизменяемые bindings, снимок одобренного шаблона, CTID,
случайный ownership nonce, поколение, desired state и журнал операций.
Резервирование коммитится **до** внешнего запроса. Provisioning, stopped,
error и неопределённые результаты продолжают занимать ёмкость; одна и та же
CTID уникальна в пределах идентичности PVE cluster, а не alias endpoint.

PVE create возвращает UPID. Потеря ответа не означает, что CT не создана:
нельзя выбрать другую CTID или повторить создание вслепую. Перезапуск сервера
принимает существующую CT только после полной проверки владельца,
unprivileged режима, storage/network/profile и членства в разрешённом пуле.
Само закрытие процесса сервера не останавливает CT.

Операция имеет отдельный durable stage/task slot. Stop task и delete task
не затирают UPID друг друга. До подтверждённого завершения текущей операции
нельзя начать другую, даже если диагностическая phase выглядит терминальной.
Reset сохраняет старую принадлежность до завершения удаления и подтверждения
отсутствия, после чего одним CAS меняет поколение/nonce и записывает новый
create intent. Ёмкость между этапами не освобождается.

Отсутствие CT в ACL-filtered inventory, HTTP 403 или HTTP 500 не доказывает,
что диск можно считать удалённым. После завершения удаления provider требует
положительную проверку глобального ID: `GET /cluster/nextid?vmid=<id>` должен
успешно вернуть именно этот свободный ID. Это наблюдение в момент проверки,
а не новая блокировка VMID на стороне PVE.

Pool drain не удаляет работающих акторов. Новые размещения и поддерживаемые
рестарты ограничиваются состоянием пула; администратор может явно включить
пул обратно. Удаление пула и роя отказывается продолжаться, пока остаются
managed allocations или неподтверждённая очистка. Увеличение доступа токена,
снятие host policy или остановка пользовательского актора не являются
автоматическим способом обойти отказ или нехватку ёмкости.
