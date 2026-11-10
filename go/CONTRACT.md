# Go-контракт эпика #27

Общий модуль `brood.dev/go` (go 1.27, go.work в корне репозитория).
Единственный источник правил — эпик #27 (`bin/brood dev bug show 27`).
Pinned Substrate: публичная версия go.ate.dev/substrate v0.6.1-0.20261023032142-4ebd668d62e3, без машинных replace.

## Владение файлами (параллельная работа)

| Пакет | Владелец (issue) |
|---|---|
| `go/brood/store/**` | #29 |
| `go/brood/auth/**` | #30 |
| `go/brood/app/**` | #31 |
| `go/brood/substrate/**`, `go/brood/kube/**` | #32 |
| `go/brood/proxy/**` | #33 |
| `go/cmd/bro/**`, `go/brood/clientapi/**` | #34 |
| `go/brood/build/**` | #35 |
| `CLAUDE.md`, `.gitignore`, `docs/GO.md` | #28 |

Никто не редактирует чужие пакеты, `go.mod`, `go.work` и `go.sum`:
недостающие зависимости перечислить в отчёте, их добавляет владелец репозитория.
Тесты — `go test ./...`; библиотеки молчат, печатают только cmd.

## API входа (общий для панели, сервера и bro)

- Cookie: сессия — `session`, CSRF — `csrf`. Обе HttpOnly, кроме `csrf`
  (панель читает значение), Secure при HTTPS, SameSite=Lax.
- Заголовок изменения: `X-CSRFToken: <значение csrf cookie>`.
- `GET /api/auth/session`
  - анонимно: `200 {"authenticated": false}` + установка csrf cookie;
  - после входа: `200 {"authenticated": true, "user": {"username", "display_name", "is_admin"}}`.
- `POST /api/auth/login` `{"username", "password"}` + X-CSRFToken
  - успех: `204`, новая `session` cookie, ротация csrf;
  - отказ: `401 {"detail": "invalid credentials"}` (без раскрытия, что именно неверно).
- `POST /api/auth/logout` + X-CSRFToken: `204`, сервер удаляет сессию, cookie чистятся.
- Ошибки — JSON `{"detail": "<английское сообщение>"}`; без трассировок и секретов.

## Управление пользователями

Только активный администратор:

- `GET /api/admin/users/{username}` — username, display_name, is_admin, disabled;
- `PUT /api/admin/users/{username}` — все три редактируемых свойства
  `display_name`, `is_admin`, `disabled` обязательны; изменение атомарное,
  успех 204. Username неизменяем. Изменение роли/блокировки отзывает сессии;
- `DELETE /api/admin/users/{username}` — 204 только если у пользователя нет
  нетерминальных роёв; иначе 409 с требованием сначала передать или удалить их.

Последнего активного администратора нельзя заблокировать, понизить или
удалить: 409 без частичного сохранения. Аноним получает 401, обычный
пользователь 403; изменяющие методы требуют Origin/CSRF.

## Удаление проекта

`DELETE /api/projects/{id}` требует активного администратора и Origin/CSRF.
Проверка роли и удаление выполняются в одной транзакции. Пока существует
любой рой проекта со state, отличным от `deleted`, возвращается 409 с причиной;
проект, членства и связи Substrate сохраняются. Сначала нужно штатно удалить
рои. Пустой проект или проект только с deleted tombstones удаляется с 204;
неизвестный проект возвращает 404. Native ресурсы не удаляются каскадом SQL.

## Отказ удаления непустого роя

Native `DeleteAtespace` атомарно отказывает для непустой области; отдельный
`substrate.ErrAtespaceNotEmpty` сохраняет исходный APIError/HTTP409. Provisioner
переводит его в `app.ErrSwarmNotEmpty`. Только этот отказ и повторная сверка
прежнего native UID откатывают локальное намерение удаления: рой снова active,
Retry пуст, владелец сохраняет API для очистки детей. Последующий штатный DELETE
после удаления детей завершается 204. Неопределённый transport failure, UID
conflict или неизвестный namespace сохраняют error/retry; автоматического
удаления workloads и присвоения чужого UID нет. Ожидающий delete заново проверяет
owner/admin и членство после блокировки проекта.

## Восстановление потерянной связи роя

Только активный администратор:

- `GET /api/admin/orphan-swarms` возвращает `{"items":[{"atespace_name":"...",
  "atespace_uid":"..."}]}`. Это только кандидаты: namespace имеет формат имени Brood,
  ещё не удаляется и не связан ни с одной строкой роя, включая deleted. Само наличие в списке
  не доказывает принадлежность: оператор сначала проверяет происхождение.
- `GET /api/admin/orphan-swarms/{swarm_id}?atespace_uid=...` проверяет ожидаемый
  UID и возвращает `atespace_name`, `atespace_uid`, `actors` (name, uid,
  template_name) и `templates` (name, uid, images). UID/имя и отсутствие
  terminating сверяются до и после чтения, отсутствие локальной связи и роль
  администратора проверяются повторно перед ответом. Это read-only provenance
  для сверки известных имён и digest до recover: не создаёт связь или native
  ресурсы, не возвращает env, argv, политики, токены, сертификаты или ключи.
  UID обязателен; уже известный namespace, включая deleted binding, даёт 409.
- `POST /api/admin/orphan-swarms/recover` принимает ровно строковые
  `project_id`, `swarm_id`, `atespace_uid`. Требуются Origin/CSRF, существующий
  проект и исходный канонический ASCII ID роя: uppercase ULID или lowercase
  UUID. Алиас другого регистра не создаёт новую локальную строку с иным URL.
  Имя namespace вычисляет сервер;
  UID и имя сверяются с native объектом. Неизвестные поля и несколько JSON
  объектов отвергаются. Поля выбора другого владельца нет.
- После повторной проверки администратора, проекта и отсутствия связи в
  транзакции создаётся active-рой с исходным ID и native UID. Владелец —
  действующий администратор, локальное имя — `recovered-<id>`; успех 201.
  Существующие записи и grants не перезаписываются. Нет native create,
  update, resume или delete: это восстановление локальных связей.
- Несовпадение UID/имени, уже существующая связь или terminating namespace дают 409; неизвестный
  проект/native namespace — 404; неправильный запрос — 400. Аноним — 401,
  обычный пользователь — 403 до обращения к Substrate. Инфраструктурные
  токены и сертификаты не возвращаются.

Для очистки проверочного namespace после подтверждённого восстановления
используется обычный API роя: удалить только его известные проверочные ресурсы,
затем пустой рой, затем проект. Автоматического удаления кандидатов нет.

## Владелец и роли роя

Ответы создания, чтения и списка роёв содержат `owner_id`, `owner_username`
и `role`: основной владелец и эффективная роль вызывающего owner/editor/viewer.
Активный администратор имеет эффективную роль owner независимо от отдельного
локального grant. Данные выдаются только после проверки доступа к рою;
членство проекта само по себе чужие рои не открывает.

- `GET /api/swarms/{id}/members` — массив `user_id`, `username`, `display_name`,
  `role`, `disabled` для явных grants. Доступен любой роли роя.
- `POST /api/swarms/{id}/members` — ровно `username`, `role`; выдача/замена
  grant активному участнику проекта или администратору, успех 204. Меняют только
  owner/administrator активного роя. Неизвестная роль/поле — 400.
- `DELETE /api/swarms/{id}/members/{username}` — отзыв grant, 204; последний
  owner не отзывается и не понижается, 409. Запись и авторизация атомарны.
- `POST /api/swarms/{id}/transfer` — ровно `username`, owner/administrator;
  новый владелец — активный участник проекта или администратор. Прежний
  основной owner-grant снимается; остальные co-owner grants сохраняются.
- `GET /api/projects/{id}/members` — массив username/display_name/is_admin/disabled
  доступен участнику проекта и администратору для выбора пользователя. Не выдаёт
  чужие рои, credentials или sessions. Постороннему проект скрыт, 404.

Viewer читает actor/template API, но не создаёт/изменяет/удаляет нагрузку и не
открывает записывающий actor proxy/terminal. Editor управляет нагрузкой, но не
ACL/передачей/удалением роя. Изменения требуют Origin/CSRF; известному reader без
owner-прав — 403, постороннему рой скрыт как 404. Native policy, включая
вложенный GoldenEgressPolicy шаблона, остаётся admin-only; native feature gates
не заменяют эту проверку. `GET /api/identities` — отдельный admin-каталог native
SA и уже встреченных principals, не превращение локальных login в native identities.

## Запросы роя

Устройство и инварианты — [MODEL.md](../docs/MODEL.md), «Запросы роя» (#209, #210).

- `GET /api/swarms/{id}/requests` — `{"results": [...]}` открытых запросов,
  `?all=1` — всех; любая роль роя. Элемент: `id`, `swarmId`, `origin`
  (`node`/`actor`), `sequence`/`nodeId` узлового запроса, `actor`
  (`name`, `incarnation`) запроса актора, `kind`, `question`, `context`,
  `options` (`label`, `outcome`), `state`, `createdAt`, после ответа
  `answeredAt`, `answeredBy` (username), `outcome`, `optionLabel`, `response`
  и у запроса актора `delivery` (`pending`/`delivered`/`failed`).
  `context` узлового запроса — отчёт шага, который ввёл граф в узел в этом
  проходе (#233): строка `Report of step <n> (node <узел>, outcome <исход>):`,
  пустая строка и отчёт как есть; без отчёта поля нет.
- `POST /api/swarms/{id}/requests/{request}/answer` — ровно
  `{outcome?, response?}`, owner/editor/administrator; 200 — запрос после
  ответа. Сбой доставки ответа актору — не отказ: `delivery: "failed"` и
  `deliveryError` из фиксированного набора. Отказы: 404 рой или запрос не
  найден (в том числе запрос чужого роя), 403 права или сессия изменились,
  409 запрос закрыт или актор устарел, 400 ответ не подходит запросу.
- `POST /api/swarms/{id}/requests/{request}/deliver` — повтор доставки
  записанного ответа актору, те же права; доставленный ответ не отправляется
  снова, запрос без доставки — 409.

Тексты отказов — фиксированный публичный набор; текст ошибок хранилища и
адаптеров клиенту не возвращается. Ответ оператора идёт только этим API:
старого `POST /api/pipeline-runs/{run}/nodes/{node}/answer` нет (#217), им
не пользовались ни `bro`, ни MCP.

## Запуск и остановка роя

Устройство и инварианты — [MODEL.md](../docs/MODEL.md), «Запуск и остановка
роя» (#211, #228, #242).

- `POST /api/swarms/{id}/start` — ровно `{definition, goal, commit?}`,
  строки, owner/editor/administrator, Origin/CSRF; 200 — `swarmId`,
  `execution`, `definition` (`name`, `version`, `revision`), `source`
  (`origin`, `commit`), `goal` — задача текущего прохода, и последний шаг
  `step` (`sequence`, `nodeId`, `state`, `detail` у шага актора, «Шаги
  актора»). `goal` обязателен на каждом запуске: непустая строка до 4096
  байт UTF-8 без управляющих символов, кроме `\n` и `\t`; она хранится у
  первого шага прохода и больше не меняется. Тело — до 64 КиБ, чтобы задача
  в пределе проходила и при экранировании каждого символа.
  Полный commit в теле пиннится без сетевого запроса. Без `commit` первый
  запуск пиннит commit, на который сейчас указывает HEAD repository проекта:
  сервер разрешает его до транзакции записи Git-доступом шлюза. Повторный
  запуск идёт на закреплённом источнике; непустой `commit` обязан с ним
  совпасть.
- `POST /api/swarms/{id}/stop` — без тела, те же права; 200 — то же тело,
  `goal` — задача остановленного прохода.

Отказы запуска ничего не пишут. 400 `invalid swarm start: expected
{definition, goal, commit?} with a definition name and a goal` — тело не
объект, неизвестное или повторное поле, не строка, нет `definition` или
`goal`. 422 `invalid swarm goal: ` с одной фиксированной причиной: `the goal
is empty` (пусто или одни пробелы), `the goal exceeds 4096 bytes`, `the goal
has control characters other than newline and tab`, `the goal contains the
message envelope closing tag`. Обе проверки — до разрешения ветки проекта:
запроса к upstream нет. 404 — рой скрыт или определения нет в
каталоге проекта, 403 — права или сессия, 409 — состояние роя или
исполнения, другой снимок, непривязанная роль или repository проекта
сменился, пока сервер разрешал ветку, 422 — неисполнимый граф или источник
(нет repository, неполный commit). Отказ разрешения ветки — `detail` с
префиксом `cannot resolve the project branch: ` и одной фиксированной
причиной, без адреса upstream, его тела и учётных данных:

- 422 `the project repository has no server Git access; bind a project Git credential or name the commit` — запрос не отправляется;
- 502 `Git upstream request failed`, `Git upstream address denied`,
  `Git upstream denied access` (401/403), `Git upstream repository not found`
  (404), `Git upstream redirect denied`, `malformed Git ref advertisement`,
  `Git ref advertisement too large`;
- 504 `Git upstream request timed out`;
- 422 `the repository has no default branch`, `the repository HEAD is not a branch`,
  `the repository HEAD is not a SHA-1 commit`.

`bro swarm start SWARM --definition NAME (--goal TEXT | --goal-file FILE)
[--commit SHA]` и `bro swarm stop SWARM` (#242) ходят в эти маршруты по bro
login сессии; рой — по ID или имени, как у `bro swarm roles`. `--goal-file`
читает файл или stdin (`-`), снимает один завершающий перевод строки и
отказывает до запроса, если задача длиннее 4096 байт; остальные правила
задачи проверяет сервер. Из тела отказа `clientapi` берёт причину, только
если это одна строка без управляющих символов до 1024 байт, равная `invalid
swarm start: ...`, `swarm management denied` или `swarm not found`, либо
начинающаяся с фиксированной части причины с подробностью: `invalid swarm
goal: `, `workflow definition is not in the project catalog: `, `swarm state
conflict: `, `swarm graph is pinned: `, `workflow graph is not executable: `,
`swarm source is invalid: `, `swarm source changed during start: `, `cannot
resolve the project branch: `, `role "`.

## Привязка ролей роя к акторам

Устройство и инварианты — [MODEL.md](../docs/MODEL.md), «Привязка ролей»
(#208, #235). Все три маршрута отвечают представлением ролей:
`{"swarmId", "execution", "graph", "roles"}`. `graph` — `null` или
`pinned` (снимок роя или определение каталога), `name`, `version`,
`revision`. Элемент `roles` по имени роли: `role`, `inGraph`, `state`
(`ready`, `unbound`, `stale`, `inactive`, `foreign`, `missing`) и у
привязанной роли `actorUid`, `actorName` (только у актора этого роя),
`actorIncarnation` — записанная инкарнация.

- `GET /api/swarms/{id}/roles[?definition=NAME]` — любая роль роя. Отказы:
  404 `swarm not found`, 404 `workflow definition is not in the project
  catalog` (в том числе определение чужого проекта), 409 `swarm graph is
  pinned to another workflow definition`, 422 `workflow graph is not
  executable`.
- `PUT /api/swarms/{id}/roles/{role}` — ровно `{"actorUid": "...",
  "actorIncarnation": N}`, инкарнация необязательна и положительна;
  owner/editor/administrator, Origin/CSRF; 200 после записи.
- `DELETE /api/swarms/{id}/roles/{role}` — те же права; 200 после записи.

Отказы изменения, без записи: 400 `invalid swarm role binding: expected
{actorUid, actorIncarnation?}` (неизвестное или повторное поле, не строка,
не положительное целое), 400 `swarm role must be a lowercase DNS label`,
403 `swarm management denied` (viewer, отзыв права или сессии), 404 `swarm
not found` (посторонний), 404 `swarm role is not bound` (`DELETE`), 409
`swarm is not active`, 409 `swarm execution is running or waiting; stop
the swarm before changing role bindings`, 409 `actor is not an actor of
this swarm` (в том числе неизвестный UID), 409 `actor is not live: it must
be provisioning or running and meant to run`, 409 `actor incarnation
changed; list the swarm actors and bind again`. Текст ошибок хранилища
клиенту не возвращается.

`bro swarm roles|bind|unbind` ходит в эти маршруты по bro login сессии;
актора по имени или UID разрешает через `GET /api/swarms/{id}/actors`
(строки `provider: proxmox`, инкарнация — `generation`). Из тела отказа
`clientapi` берёт только причину из перечисленного набора.

## Шаги актора

Устройство и порядок — [MODEL.md](../docs/MODEL.md), «Шаг актора» (#212).
Маршрута доставки нет: шаг actor-узла берёт серверный диспетчер после
коммита запуска, узлового ответа и исхода прошлого шага, а также при старте
сервера.

- Задача прохода в чтении исполнения (`GET /api/pipeline-runs/{run}`,
  `GET /api/swarms/{id}/pipeline-runs`): `goal` прогона — задача текущего
  прохода; у узла прогона `pass` — `sequence` первого шага его прохода и
  `goal` — только у этого первого шага. До первого запуска полей нет.
- Состояния шага (`state` в `step` ответа `POST /api/swarms/{id}/start` и
  `/stop`, `status` узла в `GET /api/pipeline-runs/{run}`): `pending`,
  `attesting`, `delivering`, `inflight`, `unconfirmed`, `refused`, `waiting`,
  `answered`, `completed`, `stopped`. В виде прогона `attesting`,
  `delivering`, `inflight` и `waiting` — `active`.
- `step.detail` ответа запуска и остановки и `detail` узла прогона —
  причина `refused` или `unconfirmed` для оператора; `commit` узла прогона —
  commit источника, который подтвердил workspace актора шага; `report` узла
  прогона — отчёт актора о завершённом шаге (#233), без отчёта поля нет.
- Подтверждение: сервер зовёт `workspace_attest` (`brood-session-rpc-v1`,
  docs/PROXMOX.md) с ровно `swarm`, `step` (`request_id` попытки), `origin`,
  `commit` и `challenge`, `caller` — `swarm:<id роя>`. На время вызова
  актор держит workspace-lease с операцией `workspace_attest` и ID попытки;
  его видно и после потерянного ответа освобождают так же, как
  неподтверждённый restart: `dispatch --action show|release` оператора.
  Принимается только ответ с `challenge` этой попытки; ответ другой попытки
  или инкарнации — `refused`. Остановленный за время ожидания relay рой
  `workspace_attest` не получает. Выключение сервера до вызова гостя
  возвращает шаг в `pending`, следующий старт доставляет его заново.
- Инструкция — одно сообщение канала роя: `from` — `swarm:<id роя>`, `to` —
  имя привязанного актора, `waitSec` 0. Тело: `[brood swarm <имя>] Step <n>,
  node <узел>.`, блок `Goal of this pass (started at step <первый шаг
  прохода>; earlier passes may have had other goals):` с задачей прохода
  (#242), блок `Node instruction:` с текстом узла, строка с `run_id`,
  `sequence` и `request_id` для `pipeline_visit_complete`, по строке на исход
  `success` и `fail` с тем, куда он ведёт граф, просьба приложить `report`
  (что сделано и где результат: ветка, MR, документ) и подсказка про
  `ask_operator`. Задача и текст узла обрезаны по краям от пробелов. Не
  больше 64 КиБ + 4096 байт задачи; тело с закрывающим тегом конверта
  сообщения, шаг без задачи прохода или такое тело, чей NDJSON у гостя
  (auth-кадр, конверт, экранированное тело) превысит 128 КиБ, не
  отправляется (`refused`). Ни задача, ни текст узла не граница
  безопасности.
- Ответ гостя на сообщение канала роя (`brood-swarm-msg`) — `{ok, verdict}`
  и необязательное `notSent` (#244): `true` — отказ до записи в сессию
  Claude, сообщение заведомо не ушло. Инструкцию с таким ответом сервер
  повторяет каждые 3 секунды до 120 секунд, перепроверяя шаг перед каждой
  отправкой, затем шаг `refused` с `detail` `instruction not sent: the actor
  session did not become ready in 120 seconds (...)`. Ответ без поля —
  неизвестный исход, как раньше. `send` оператора (вердикт гостя, исход
  `unconfirmed`) и доставка ответа на запрос роя (`delivery: "failed"`,
  повтор — `/deliver`) такой отказ не повторяют и отвечают, как раньше.
- `pipeline_visit_complete` принимается от актора шага той же инкарнации с
  тем же `request_id`, пока он привязан к роли узла и рой идёт; шаг роя с
  источником — только с записанным подтверждением. Иначе гость получает
  `pipeline completion conflict` или `pipeline completion unavailable`.
- Необязательный `report` — до 16384 байт UTF-8 без управляющих символов,
  кроме перевода строки и табуляции; иначе `refused` с текстом
  `invalid pipeline report: ...`, исход не записан. Отчёт хранится один раз
  вместе с исходом: точный повтор (тот же исход и отчёт) успешен, другой
  отчёт к завершённому шагу — `pipeline completion conflict`.

## Ёмкость проекта

`GET /api/projects/{id}` доступен участнику/администратору и возвращает id/name,
`capacity`, `usage`, `usage_available` и при необходимости `usage_error`.
Capacity содержит `worker_selector` (точные label-пары) и nullable decimal strings:
`max_swarms`, `max_actors`, `max_cpu_millicores`, `max_memory_bytes`. Null — без
ограничения, `"0"` — запрет резерва. CPU задаётся в millicores, память в bytes.
Decimal strings сохраняют точность до int64, без JSON float/scientific notation.

`PUT /api/projects/{id}/capacity` принимает те же поля, полностью заменяет
свойства и требует активного администратора и Origin/CSRF; успех 204. Неуказанный
предел становится null. Допускаются только неотрицательные canonical decimal
strings до int64 и не более 10 Kubernetes labels. Неправильные значения/поля —
400, неизвестный проект — 404, обычный пользователь — 403. Роль повторно
проверяется в SQL-транзакции, изменение сериализовано с workload admission.

Usage содержит decimal strings `swarms`, `actors`, `cpu_millicores`, `memory_bytes`.
Это общий расход проекта, без имён/ID чужих роёв, шаблонов и акторов. Все
неудалённые состояния роя занимают слот; suspended actors сохраняют ресурсы.
Pending golden учитывается до native создания, без двойного счёта появившегося
актора. При неизвестном inventory расход не заменяется нулём: `usage=null`,
`usage_available=false`, нейтральная причина без приватных имён. Пустой проект
имеет известный нулевой расход даже без настроенного Substrate.

Создание роя проверяется до provisioning и атомарно при вставке. Actor create,
update/template replacement и resume проверяют общий actor/CPU/memory резерв
при наличии соответствующих пределов; отказы 409 называют ресурс, limit/used/requested.
После снижения предела уже существующий неизменный или сокращающийся резерв
допускается, увеличение сверх предела — нет. Существующая нагрузка автоматически
не удаляется, не приостанавливается и не переводится на другой runtime.

Worker selector по умолчанию берётся из шарда, явно заданный обязан сохранить
его labels; противоречащий selector шаблона отвергается. Native UID namespace и
права проверяются перед записью, в том числе после блокирующих upstream чтений.
Неизвестный inventory ограниченного шарда даёт 503, а не свободную ёмкость.
Golden-disabled template/push не расходует actor slot, но сохраняет placement
и права. Шаблон, на который ссылается обычный актор, не удаляется: 409 до
native deletion; проверка сериализована с созданием/изменением акторов. Managed
golden удаляется штатным native workflow вместе с шаблоном. Это не позволяет
сменить template resources под существующим актором через delete/recreate имени.

Native template всё равно требует положительные CPU/memory limits, даже у
неограниченного шарда: это контракт pinned upstream validator, не квота Brood.
Если resume требует установить default worker selector, выполняется whole-object
UpdateActor с исходными UID/version, затем ResumeActor. Это два native RPC, не
транзакция: при последующем отказе resume сохранённый selector не откатывается
поверх возможных конкурентных изменений. Перед каждым native write права
проверяются повторно. Прямой native API и несколько независимых экземпляров сервера не
становятся распределённой quota-транзакцией: admission сериализован внутри
одного процесса Brood и считает актуальные native записи без второго счётчика.

## Восстановление ёмкости и отказ размещения

`POST /api/workers/{name}` принимает `{"action":"drain"}` или
`{"action":"replace"}`. Обе операции требуют активного администратора и
обычной защиты Origin/CSRF. `drain` — необратимый native переход в `DRAINING`,
а не пауза воркера и не ожидание автоматического возврата. Сам по себе этот
вызов не удаляет pod. Повторно сделать ту же запись Worker активной нельзя.

`replace` возвращает 204 после принятого Kubernetes удаления, не после
регистрации замены. Допускается только `DRAINING` в namespace установки
`brood` с нулевыми actor/CPU/memory allocations; иначе 409 без удаления.
Имя и UID pod читаются из native Worker, pod должен иметь метку
`ate.dev/worker-pool`. Kubernetes DeleteOptions содержит UID precondition,
поэтому замена с тем же именем не удаляется вместо прежнего pod. Deployment
создаёт новый pod и новую запись Worker; клиенты перечитывают список до
готовности. Пул и его runtime не меняются, акторы не запускаются автоматически.
Сервисный аккаунт получает namespaced `pods: get/list/delete`; RBAC не умеет
ограничивать delete по метке, проверку метки выполняет библиотека.

Точный native отказ resume `no worker has room for the actor` сохраняет
исходный HTTP-статус и получает `code=actor_placement_unavailable` вместе с
английским `detail`. Обычному участнику возвращается общая причина без чтения
глобального списка воркеров. Активному администратору добавляется `placement`:
`workers` с `name` и `reasons`, `noWorkers`, `templateSelector`, `actorSelector`,
`unchecked`. Причина содержит `code`, для неактивного воркера — native `state`,
для CPU/памяти/слотов — decimal strings `required` и `available`. CPU измеряется
в millicores, память — в bytes; свободный остаток может быть отрицательным.

Оба selector применяются одновременно. Проверяются регистрация, состояние и
видимые ресурсы, но не воспроизводится весь native scheduler: отсутствие
видимых причин не означает успех, ограничения snapshot/runtime могут остаться
неизвестными. Ошибка ListWorkers не превращается в пустой список. Admin-флаг
перечитывается до и после RPC, native namespace UID проверяется вокруг чтений.
Посторонние native отказы сохраняют прежний контракт без этой диагностики.

## Сборка и push

`bro build [target]` читает текущий `./Broodfile`, синхронизирует декларацию
проекта через текущую bro login сессию до Docker, затем сохраняет OCI-архив
с JSON-артефактами атомарно. Registry для сборки не нужен. `--plan`/`--export`
остаются офлайн без синхронизации; обычная сборка в guest-режиме не использует
операторскую сессию и отказывает. `--project NAME`/`BUILD_PROJECT` задаёт имя
серверного проекта, не путь. `--ref`, `--registry`, `--atespace` удалены.
Справка опций — `bro build --help`, не корневая справка.

`PUT /api/projects/sync` принимает `{name, repository, description?, capacity?, git_credential?}`.
Repository — канонический HTTPS URL либо явная пустая строка. На клиенте
отсутствующее поле Broodfile разрешается из Git origin; обычные SSH/SCP origins
нормализуются в HTTPS. Capacity имеет тот же wire shape, что API ёмкости.
Пропущенные description/capacity сохраняются, явная capacity заменяется
полностью. `git_credential` — имя серверного Git credential проекта или `""`
(снять ссылку); отсутствующее поле сохраняет ссылку, непустой ссылке нужен
непустой repository. Создание — 201, обновление/no-op — 200, тело
`{id,name,repository,created,changed,origin_mode,git_credential,git_credential_bound}`. Создание и изменения требуют
администратора, no-op допускает участника проекта; сессия/права перепроверяются
в транзакции. Идентичный повтор не записывает строки БД и не создаёт дубликаты.
Членство, секреты, рои и существующие checkout акторов не меняются.
Назначение repository не ограничено BROOD_GIT_REMOTES и не выдаёт сетевых прав:
режимы gateway/direct по-прежнему проверяются отдельно.

`bro push [target] --project NAME` использует текущую bro login сессию. Назначение
разрешается в доступный активный swarm; сервер всегда назначает atespace сам.
Base API: `/api/swarms/{id}/builds/{environment}`:

- `HEAD /blobs/{sha256}` — 200/404, только свой repository.
- `POST /uploads` — 201 `{upload_id}`, upload связан с точной сессией, TTL 30 минут.
- `PATCH /uploads/{upload_id}` — octet-stream, Content-Length, chunk до 32 MiB; 204.
- `PUT /uploads/{upload_id}?digest=sha256:...` — finalization, 204.
- `PUT /manifests/{sha256}` — OCI manifest/index до 4 MiB, проверка digest и
  связанных дескрипторов в своём repository; 200 `{image}`.
- `POST /templates` — `{image, actor_template, egress_policy}`; 200
  `{image, template, project}`. Image/atespace server-scoped, идентичный retry
  идемпотентен; другое содержимое существующего имени — 409. Акторы не создаются.

Все запросы используют обычные session/CSRF и owner/membership/admin проверки.
Публичного `/v2/`, Docker credentials и отдельного push bearer нет. Registry URL
остаётся серверной настройкой. Потоки ограничены размером, временем и количеством
upload state; приватные Location/registry errors не передаются клиенту.
Template-only push нормализует unspecified golden mode в DISABLED и отказывает
explicit ENABLED. Runtime EgressPolicy не преобразуется в preview golden policy:
`egress_policy_deferred: true` и CLI-сообщение явно показывают, что правила из
локального egress-policy.json нужно применить при будущем создании actor.

## Соглашения

- Ошибки: `fmt.Errorf`/ошибки-значения; отказы CLI — строкой, без паник.
- Пароли: Argon2id (golang.org/x/crypto/argon2), параметры в кодировке хэша.
- ID: строковые ULID/UUID; `atespace_name = "swarm-" + <id без дефисов, lower>`.
- SQL: явные запросы, встроенная текущая схема для новой базы, без импорта
  legacy runtime и произвольных исторических миграций; SQLite WAL
  (modernc.org/sqlite, без cgo). Точно распознанная прежняя схема расширяется
  ACL в одной BEGIN IMMEDIATE: swarm_members, owner-grants из owner_id,
  уникальное имя не удалённого роя в проекте, затем project_capacity с
  PRAGMA user_version=2. Известная версия 1 расширяется до 2 в той же транзакции.
  Пользователи, сессии, членства, рои и grants сохраняются. Конфликтующие имена,
  неизвестная версия, зарезервированная таблица до её версии или повреждённая
  схема — отказ с полным откатом. Версия 2 проверяется без восстановления
  потерянных grants или таблиц. Повторное открытие идемпотентно.
- Свойства шарда: GetProjectCapacity возвращает selector и nullable int64-пределы
  числа роёв, акторов, CPU в millicores и памяти в bytes. Отсутствующий предел
  не ограничивает расход; явный ноль запрещает резерв. Отрицательные значения
  запрещены. Worker selector — точные Kubernetes label-пары; дополнительная
  привязка не хранится во втором счётчике. Для существующего проекта без строки
  свойств возвращаются неограниченные defaults; неизвестный проект — ErrNotFound.
  SetProjectCapacity полностью заменяет свойства в одной транзакции; удаление
  пустого проекта удаляет строку свойств через FK. Это слой хранения, не проверка
  квоты и не разрешение на каскадное удаление живых роёв.
- ACL хранилища: owner/editor/viewer, хотя бы один owner; изменение роли,
  отзыв и передача атомарны. owner_id пока является совместимой проекцией
  одного из owner. Удаление пользователя запрещено при любой owner-роли
  в не удалённом рое. Политика выдачи и проверки доступа — прикладной слой.
- Время: UTC; парсинг/печать RFC3339.
