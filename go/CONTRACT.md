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

## Сводка в списках

Строки списков несут сводку (#234); каждая сводка — одна выборка хранилища
на весь список, без запроса на рой или проект.

- `GET /api/projects/{id}/swarms` — у строки `execution` (исполнение графа:
  `idle`, `running`, `waiting`, `succeeded`, `failed`, `stopped`; не путать с
  `state` инфраструктуры), `openRequests` — число открытых запросов роя,
  узловых и актора, то же, что `GET /api/swarms/{id}/requests` без `?all`,
  и `undeliveredAnswers` — число ответов актору, ждущих повтора доставки
  (#266): элементов `GET /api/swarms/{id}/requests?all=1` с `origin: "actor"`,
  `state: "answered"` и `delivery` `failed` или `unconfirmed`. Доставка
  `pending` и `delivered` в него не входит; открытые запросы считает только
  `openRequests`.
- Там же (#268): `definition` — `{name, version, revision}` определения
  workflow, из которого снят снимок графа роя: личность, записанная первым
  запуском, а не текущее определение каталога (новая публикация её не
  меняет, #218). У роя без записанной личности, в том числе ни разу не
  запущенного, поля нет. `memberCount` — число явных участников роя любой
  роли, длина `GET /api/swarms/{id}/members`; эффективные права
  администратора без явной роли в него не входят. `managedActorCount` —
  число managed-акторов роя (провайдер `proxmox`) в любой фазе, кроме
  `deleted`: элементов `GET /api/swarms/{id}/actors` с `provider: "proxmox"`.
  Native-акторы Substrate в него не входят: это не общее число акторов роя.
  Числа native-акторов и шаблонов в строке нет: их знает только Substrate,
  а строка списка запросов к нему не делает. `updated_at` строки — не момент
  прогона: окончание прохода succeeded или failed его не меняет, а смена
  владельца и инфраструктуры меняет.
- Там же (#268): `executionAt`, момент последнего исполнения роя: RFC 3339
  UTC с долями секунды, как `finishedAt` шага. Это последняя из трёх границ
  прохода: запуск (начало текущего прохода, `running` или `waiting`),
  остановка (`stopped`, в том числе посреди шага, чей `finishedAt` пуст) и
  окончание прохода `succeeded` или `failed`, где момент равен `finishedAt`
  шага, исход которого кончил проход (ответ оператора или исход актора,
  ведущий в выход, или терминальный отказ без ребра). Переходы между
  `running` и `waiting` внутри прохода, смена владельца, состояния
  инфраструктуры и atespace его не меняют. Поля нет у роя без прохода и у
  роя, чей момент база версии 27 не знала точно и который с тех пор не
  проходил границы прохода: проход, начатый шагом актора, и остановка на
  шаге, который остановка не закрыла ([MODEL.md](../docs/MODEL.md), «Версия
  схемы 28»).
- `GET /api/projects` — у строки `swarmCount`: длина списка роёв проекта у
  того же вызывающего (администратору — все неудалённые рои, участнику — рои
  с его ролью).

Сводка есть только у строк, которые список уже показывает: видимость роя она
не расширяет. Ответы создания и чтения одного роя и создания проекта этих
полей не несут.

## Отказы команд

`POST /api/commands/{tool}` отвечает на ошибку исполнения JSON с `detail`,
как прежде: это текст ошибки сервера, у внутреннего сбоя он может нести
подробности хранилища или провайдера, и клиент его не показывает.
Сознательный отказ команды (`command.Refusal`: публичная причина и класс
ошибки для статуса, без ошибки, которая к нему привела) добавляет `reason`
(#276):

```json
{"detail": "conflict: update refused: actor \"worker2\" did not report ...",
 "reason": "update refused: actor \"worker2\" did not report ..."}
```

- `reason` написан для вызывающего (оператор, модель) и не несёт текста
  внутренней ошибки: отказ, заменивший сбой хранилища или провайдера,
  называет фиксированную фразу. Сбой хранилища, провайдера или гостя и
  неизвестный исход `reason` не несут.
- `reason` — UTF-8 до 1024 байт без управляющих символов, в том числе
  перевода строки и табуляции; иначе поля нет, `detail` остаётся.
- `detail`, статус и заголовок прежние: у отказа, который писался
  `fmt.Errorf("%w: ...")`, `detail` начинается с класса (`conflict: `,
  `not found: `), `reason` — без него; 403 у класса `not allowed`, 404 у
  `not found`, иначе 409; `X-Brood-Command-Outcome: unconfirmed` ставит
  неизвестный исход, а не `reason`.
- `reason` несут проверки области команды (`swarm not found`, `swarm is not
  active`, `administrator access required`, `a swarm scope is required`),
  жизненный цикл актора и `add`, `update` (#133), владение master (#152,
  #267), сессии и workspace, `dispatch` и `landing` с держателем аренды
  (#272), `send` отключённому адресату. Административные команды (`models`,
  `profile`, `project_*`, `node*`, `server_diagnose`), `agents` и отказ гостя
  в сессионной операции его пока не несут.
- Гость получает тот же текст отказа через relay, как прежде; из других
  путей API `reason` отдаёт только профиль Telegram (#281).

## Явный владелец проекта

`GET /api/projects/{id}/owner` читают текущий admin и участники проекта. Ответ:
`{projectId, viewerUid, revision, ownerUid, ownerUsername, updatedBy, updatedAt}`.
`viewerUid` — UID текущей authenticated human session, не имя и не UID владельца
из input; ответ персонализирован и имеет `Cache-Control: no-store`. Revision 0 и
пустые owner/author/time означают отсутствие назначения. Положительная revision
с пустыми ownerUid/ownerUsername означает снятое назначение либо удалённый
аккаунт владельца. Чужой или отсутствующий
проект — 404, отсутствие human session — 401.

`PUT /api/projects/{id}/owner` доступен только текущему installation admin с
Origin/CSRF и живой сессией. Тело не больше 16 KiB, ровно `{revision, username}`:
неотрицательная integer revision и строка username. Неизвестные/повторные поля,
null, неверный регистр, повреждённый UTF-8 или второй JSON-документ — 400. Непустой username
должен разрешаться в активного текущего участника этого проекта; пустой явно
снимает owner. Изменение — CAS по текущей revision, устаревшая — 409, без retry.
Успех 200 возвращает новый снимок с revision + 1. Account/admin/session и target
membership проверяются внутри записи; отказ не меняет назначение.

Назначение даёт неадминистратору только право редактирования workflow policy
этого проекта при текущем членстве. Не даёт installation-admin, управления
ownership или новых прав на другие административные действия. Отсутствие записи,
роль owner в рое и прежнее авторство policy не дают grant. Preserving migration
62→63 добавляет пустую таблицу без вывода владельцев из старых данных. Панель
показывает сведения всем читателям проекта, форму назначения/отзыва — только
admin, а policy editor — admin или актуальному явному owner; неизвестная ownership
не открывает права owner.

## Политика workflow проекта

`GET /api/projects/{id}/workflow-policy` — текущий default политики; читают
участники проекта и администратор. Ответ: `projectId`, числовая `revision`,
`rules`, `createdBy` (ID автора) и `createdAt`. Ненастроенный существующий проект
возвращает revision 0, пустые rules и строки автора/времени, а не 404.
Чужой/отсутствующий проект — 404, без сессии — 401. Ответ `Cache-Control: no-store`.

`PUT /api/projects/{id}/workflow-policy` — текущий администратор либо явный
активный владелец этого проекта с текущим членством, Origin/CSRF и действующей
сессией; тело ровно `{revision, rules}` с явной текущей
revision и массивом правил. Неверные типы, null, неизвестные поля, повторы ключей
(в том числе внутри правила), регистровые варианты и второй JSON-документ — 400.
Тело ограничено 1 МиБ; правила — 64 правилами, ограничениями evaluator и
131072 байтами JSON после кодирования. Неверные правила или превышение размера
сохраняемого набора — 422 без отражения входного тела. Правило имеет `id`,
`paths?`, `actions?`, `require` (`human`, `independent_review`, `deny`) и
`reviewRole?`; независимому review роль обязательна.

Запись атомарно добавляет неизменяемую версию и сдвигает head по revision CAS.
Устаревшая revision — 409, не перезапись и не автоматический retry на свежую
версию. Успех — 200 с новым снимком. Явный пустой массив rules добавляет новую
пустую версию, не удаляет историю. Автор, текущая admin либо owner/membership
identity и session authority заново проверяются в транзакции после чтения тела;
потеря полномочий — 403 без записи.
Изменение default не переписывает policy уже запущенного прохода и не даёт
разрешения активному candidate; политика закрепляется отдельно при запуске.

## Запросы роя

Устройство и инварианты — [MODEL.md](../docs/MODEL.md), «Запросы роя» (#209, #210).

- `GET /api/swarms/{id}/requests` — `{"results": [...]}` открытых запросов,
  `?all=1` — всех; любая роль роя. Элемент: `id`, `swarmId`, `origin`
  (`node`/`actor`/`policy`), `sequence`/`nodeId` узлового запроса, `actor`
  (`name`, `incarnation`) запроса актора, `kind`, `question`, `context`,
  `options` (`label`, `outcome`), `state`, `createdAt`, после ответа
  `answeredAt`, `answeredBy` (username), `outcome`, `optionLabel`, `response`
  и у запроса актора `delivery`: `pending`, `delivered`, `failed` (ответ
  заведомо не ушёл, повтор безопасен) или `unconfirmed` (ушёл без
  подтверждения, ответ мог дойти, повтор может доставить его дважды, #260).
  `context` узлового запроса — отчёт шага, который ввёл граф в узел в этом
  проходе (#233): строка `Report of step <n> (node <узел>, outcome <исход>):`,
  пустая строка и отчёт как есть; без отчёта поля нет.
- `POST /api/swarms/{id}/requests/{request}/answer` — ровно
  `{outcome?, response?, revision?}`, owner/editor/administrator; 200 — запрос после
  ответа. Сбой доставки ответа актору — не отказ: `delivery: "failed"` или
  `"unconfirmed"` и `deliveryError` из фиксированного набора; у
  `unconfirmed` — `delivery is unconfirmed: the answer may have reached the
  actor, and a retry may deliver it twice`, у отказа гостя до записи в сессию
  — `the answer was not sent: the actor session did not receive it`. Сервер
  ждёт доставку до 40 секунд: гость ждёт квитанцию до 15. Отказы: 404 рой
  или запрос не найден (в том числе запрос чужого роя), 403 права или сессия
  изменились, 409 запрос закрыт или актор устарел, 400 ответ не подходит
  запросу.
- `POST /api/swarms/{id}/requests/{request}/deliver` — повтор доставки
  записанного ответа актору из `pending`, `failed` или `unconfirmed`, те же
  права; доставленный ответ не отправляется снова, запрос без доставки —
  409. Повтор из `unconfirmed` может доставить ответ дважды; не ушедший
  повтор оставляет `unconfirmed`, и `deliveryError` добавляет `; an earlier
  delivery is unconfirmed: the answer may have reached the actor, and a
  retry may deliver it twice`. Это предупреждение относится к legacy-пути.
  Для retained journal (#291) неизвестный эффект допускает только read-only
  probe прежнего ID. Он не пишет повторно в session, а подтверждает лишь
  сохранённую квитанцию точного journal. Смена или отсутствие journal не дают
  fallback на legacy-send. Telegram `sendMessage` не получает такого протокола.

Операторский CLI: `bro swarm requests SWARM [--all]` и
`bro swarm answer SWARM REQUEST [--outcome VALUE] [--response TEXT] [--revision N]`.
Для policy-запроса передайте `--response yes` или `--response no` и `--revision 1`.

Policy-запрос (#290) имеет `id: "policy:<candidate ID>"`, `origin: "policy"`,
`kind: "confirm"`, `revision: 1` до ответа и `2` после. Ответ — ровно
`{response: "yes" | "no", revision: 1}` без `outcome`: клиент передаёт увиденную
ревизию, не подбирает новую при конфликте. Повтор того же ответа того же
пользователя возвращает первоначальную запись; другой ответ или ревизия — 409.
Это не сообщение актору и не исход graph-узла; `deliver` возвращает 409.
Подтверждение относится только к этому candidate; другие обязательства могут
по-прежнему блокировать автоматическое продолжение. Изменение текущего default
не заменяет политику прохода или сохранённое решение.

`GET /api/swarms/{id}/requests/policy:{candidate}/policy` — полный неизменяемый
снимок оснований решения, доступный любой текущей роли роя, в том числе viewer.
Ответ: `id`, `version: 1`, `swarmId`, `sequence`, `pass`, `policyRevision`,
`boundary` (`entry`/`result`), `createdAt`, `target`, `targetKind`, `initial`,
`actionsKnown`, `actions`, `rules` (правила закреплённой политики), `evidence`
(`id`, `known`, `complete`, `artifact`, `producerUid`, полный `paths`) и
`decision` (`state`, `matches` с `ruleId` и полным `paths`, `human`, `review`,
`denied`, `unknown`, `notApplicable`). Пустые массивы остаются массивами.
Это исходная оценка, не текущее разрешение перехода: ответ оператора, остановка
и новые defaults её не переписывают. Начальная граница не изобретает пустой
успешный artifact: `initial` и признаки известности evidence передаются явно.
Чужой candidate, другой рой или неполитический request — 404; без сессии — 401.
Маршрут read-only: изменяющий метод авторизованного читателя — 405 с `Allow: GET`.
Чтение не создаёт ответ, шаг, lease и не обращается к гостю.

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
  Полный commit в теле пиннится на новый проход без сетевого запроса. Без
  `commit` запуск, первый и повторный, пиннит commit, на который сейчас
  указывает HEAD repository проекта: сервер разрешает его до транзакции
  записи Git-доступом шлюза (#229). `source` — origin роя и commit текущего
  прохода. Повторный запуск с другим определением или при repository
  проекта, отличном от origin роя, — 409 `swarm graph is pinned: `.
- `POST /api/swarms/{id}/stop` — без тела, те же права; 200 — то же тело,
  `goal` — задача остановленного прохода. При незавершённых детях только
  owner/administrator может заморозить диспетчер с журналом `pass_stop`:
  родитель остаётся `unconfirmed`, дети и их holds не отменяются. Новый
  автоматический проход до сверки — 409 с причиной
  `outstanding child obligations require explicit reconciliation before automatic progression`.

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
swarm start: ...`, `swarm management denied`, `swarm not found` или
`outstanding child obligations require explicit reconciliation before automatic progression`, либо
начинающаяся с фиксированной части причины с подробностью: `invalid swarm
goal: `, `workflow definition is not in the project catalog: `, `swarm state
conflict: `, `swarm graph is pinned: `, `workflow graph is not executable: `,
`swarm source is invalid: `, `swarm source changed during start: `, `cannot
resolve the project branch: `, `role "`.

## История дочерних групп

Human API #289 требует текущую login-сессию и право видеть рой. Членство в
проекте без роли этого роя не даёт доступ; скрытый рой — 404, анонимный запрос
— 401. Ответы имеют `Cache-Control: no-store`. Чтение не выдаёт гостевые права,
не снимает holds и не зависит от готовности или текущей инкарнации старого актора.

- `GET /api/swarms/{id}/children[?before=N&limit=N]` — `items` и `nextBefore`.
  Порядок — убывание parent sequence; `before=0` или отсутствие cursor означает
  начало, иначе sequence строго меньше cursor. `limit` по умолчанию 25, диапазон
  1–100; cursor — неотрицательный int64. `nextBefore=0` — конец истории.
  Неизвестные, повторные, пустые или некорректные параметры — 400.
- Metadata группы: `parentSequence`, `pass`, `parentUid`, `parentIncarnation`,
  `mode`, `total`, `quorum`, `state`, `revision`, `interrupted`, `registered`,
  `unregisteredFailed`, `executorFailed`, `open`, `started`. `unregisteredFailed`
  считает явные отказы job до регистрации и не входит в `registered`.
  `executorFailed` — отказы уже зарегистрированных pending/running детей,
  подмножество `registered`, а не дополнительные слоты. `open` включает
  отсутствующие слоты без явного отказа и незавершённых детей без server failure,
  у aborted группы — 0. `started` считает running/completed детей, а не
  доказывает внешний эффект. Payload и отчёты не входят в страницы metadata.
- `GET /api/swarms/{id}/children/{sequence}` — те же поля, `readiness`,
  `abortedBy`/`abortedAt` при наличии и массив `children`. Sequence — положительный
  int64, query parameters не поддерживаются: неверный запрос — 400,
  отсутствующая группа своего роя — 404.
- Ребёнок: `id`, `requestId`, `role`, `executorUid`, `executorIncarnation`,
  `state`, `delivery`, `deliveryDetail` при наличии, `task`, `outcome`/`report`
  при наличии, `revision`, `createdAt`, `finishedAt` при наличии. Исходные task и
  report возвращаются без переписывания. Optional `executorFailure` содержит
  точные `intentId`, `childId`, `requestId`, `reason`, `failedAt` отдельного
  серверного решения, не заменяя actor state/outcome/report или delivery.
  В контракте нет session hashes, credential references или внутренних
  principal-объектов.

`readiness` — результат оценки join, не утверждение, что родитель успешно
завершён. `interrupted=true` сохраняет неопределённость и после позднего отчёта.
Исторические UID/incarnation не подменяются актуальными после reset.

`POST /api/swarms/{id}/children/{sequence}/abort` принимает только
`{"revision":1,"confirm":true}` с текущими owner/admin web session и CSRF.
Разрешена лишь неполная группа без running/completed детей, при точной revision;
отказ CAS или неподходящее состояние — 409. Viewer может читать историю, но
получает 403 на abort. Сохранённые policy, payload и cancelled записи не удаляются.
Это не API сверки или отмены неизвестного внешнего эффекта.

## Трасса прохода

`GET /api/swarms/{id}/passes/{pass}` (#293) читает сохранённый проход одним
согласованным snapshot. `pass` — канонический положительный int64, номер первого
visit с goal; sequence внутри прохода не является его альтернативным ID.
Неверный номер/формат — 400, отсутствующий проход своего роя — 404. Требуются
текущая human login-сессия и видимость роя, проверяемая повторно перед выдачей:
viewer может читать, член проекта без grant роя получает 404, анонимный клиент
или один machine Bearer без human-сессии — 401. Метод только GET/HEAD; прочие
методы — 405 с `Allow: GET, HEAD`. Ответы имеют `Cache-Control: no-store`.

Единственный query parameter — `download=0|1`, без повторов; неизвестный,
некорректный или пустой параметр — 400. `download=1` отдаёт тот же JSON с
`Content-Disposition: attachment; filename="brood-pass-N.json"`; имя не задаётся
вызывающим. `version: 1`, `swarmId`, `pass`, `goal`, `source` (origin/commit
именно прохода), `definition`, исходный `graph` текстом, `current`, `steps`,
`requests`, `unscopedActorRequests`, `gaps`. `execution` присутствует только у
текущего прохода: статус нового запуска не выдаётся за исторический.

`steps` идут по sequence. В каждом есть nodeId/state, сохранённые времена,
стабильная actor UID/incarnation (опциональный currentName — только текущая
подпись), requestId/attestedCommit и actual outcome/report/detail при наличии.
`reportedResult` отдельно хранит ещё не применённый completion report;
`resultObservation` — только id/state/detail/revision, `artifactEvidence` —
commit/baseCommit/producer UID/incarnation и полные пути. Root RPC lease, challenge,
acceptance nonce, HMAC signature, machine token и session hashes в DTO не входят.
`policyCandidates` — исходные immutable решения, не текущее разрешение действовать.
`children` использует публичную проекцию истории дочерних групп.

`instruction` и `deliveryHistory` сохраняют исходный frame текстом и все attempts:
number/mode/createdAt/deadlineAt, `resultRetained`, исход/detail/observedAt при
наличии. `childInstructions` сохраняет такую же проекцию typed child delivery.
Отсутствующая квитанция не выдумывается как известный `not_sent`.
`ephemeralJobs` показывает сохранённый intent, необязательные allocation и
`childId`, а также раздельные `failure` до регистрации и `executorFailure`
после регистрации; эти факты не являются actor report или cleanup grant.
Actor-вопрос вне scoped ephemeral child остаётся глобальным; `createdDuring` описывает точные pass и observed
sequence при создании, а не владение graph-шагом. Вопросы без такого контекста
перечислены отдельно и не приписываются проходу по timestamps. `answeredByUid`
остаётся сохранённой личностью автора, а не сегодняшним username.

Лимит — 4096 записей visits и вопросов суммарно; бюджет материала — 32 MiB. Превышение
возвращает 413 с причиной и без частичного JSON/download header, не усекает frames
или reports. Не сохранённая legacy-инструкция или неизвестное время явно названы
в gaps. Чтение/выгрузка не повторяет доставку или workspace observation, не закрывает
join, не применяет policy и не выдаёт replay/landing authority. Это read-only
trace/export API; страница панели открывается из истории проходов. Replay eval
и сравнение не запускаются чтением трассы.

### Сравнение сохранённых проходов

`GET /api/swarms/{id}/passes/{pass}/compare?swarm=ID&pass=N` (#293) сравнивает
reference из адреса с candidate из query. Оба номера — канонические положительные
int64. `swarm` и `pass` обязательны ровно по одному разу; опциональный `download`
принимает только `0|1`. Неизвестные/повторные/неполные параметры — 400. Невидимая
любая сторона или отсутствующий проход — 404; анонимный клиент или machine Bearer
без human-сессии — 401. Текущий owner/editor/viewer может читать; членство в проекте
не заменяет grant роя. Видимость обеих сторон проверяется до единой транзакции
чтения и повторно перед выдачей. Методы только GET/HEAD, иначе 405 с Allow.

JSON содержит `version: 1`, `alignment: "visit-position"`, `reference` и `candidate`
(swarmId/pass), `sameGoal`, `sameSource`, `sameDefinition`, `stepCount`,
`childTaskCount` и `steps`. Счётчики имеют reference/candidate/delta, delta всегда
candidate-reference и считает сохранённые записи, не неучтённую работу.
Сопоставление по позиции не ищет похожие узлы и не доказывает эквивалентность:
разные nodeId дают `not_comparable`, отсутствующая сторона — `node: "missing"`.

Строка шага сохраняет sequence/nodeId/state каждой стороны; actual outcome/report
и reportedOutcome/reportedReport — отдельно. Статусы сравнения: `same`, `different`,
`unknown` при отсутствии сохранённого значения и `not_comparable` для разных узлов
или отсутствующей стороны. Неприменённый completion не подставляется в actual.
`durationMillis` — интервал корректных сохранённых времён; отсутствует для
неполных, повреждённых, отрицательных или переполняющихся интервалов. Разница
`durationDeltaMillis` есть только для того же nodeId с известным временем обеих
сторон. Это elapsed wall-clock time, не model latency.

Ответ `no-store`, `nosniff`, application/json. `download=1` отдаёт те же данные с
фиксированным `attachment; filename="brood-pass-comparison.json"`. Лимиты трасс
и 32 MiB результата действуют без усечения: превышение — 413 с причиной, без
частичного JSON или download header. Root authority material не включается.
Сравнение не наблюдает workspace заново, не повторяет доставку, не даёт eval
provenance или разрешения landing/разрушительных действий.

Operator CLI `bro swarm compare SWARM PASS OTHER_SWARM OTHER_PASS [--json]`
разрешает каждое имя роя независимо среди доступных, либо принимает ID. Номера
проходов — канонические положительные int64. Текст явно показывает unknown и
разделяет actual/reported, пользовательские строки экранированы для терминала;
`--json` печатает полный документ. Ошибки аргументов не делают запросов, guest
mode запрещён, ошибочный API-ответ не печатается частично. Сравнение read-only,
повторов или fallback нет. Панель использует тот же endpoint на странице
`/#/swarms/{id}/passes/{pass}/compare?swarm=ID&pass=N`: явный выбор кандидата,
пары сохранённых записей, исходные отчёты и JSON download, без запуска работы.
Вердикт сравнения явно подписан и не выдаётся за actual outcome любой стороны.
Replay eval не запускается сравнением; у него отдельный двухэтапный контракт.

### Изолированный eval

`POST /api/swarms/{id}/eval` принимает ровно
`{referenceSwarmId, referencePass, definition, model}`. Human session и CSRF
обязательны для мутаций; текущий launcher должен управлять candidate и reference.
Candidate — новый рой отдельного проекта с тем же repository origin, без истории
managed actors (включая tombstones), native actors и pending reservations. Binding
создаётся под общей project admission gate до загрузки любого candidate actor.

Reference pass должен быть завершён. Binding неизменно сохраняет его goal/commit,
снимок candidate definition (name/version/revision/graph), explicit ASCII model ID
и `nonProduction: true`. Credentials и сетевые настройки не копируются. После
binding можно настроить изолированные managed actors и их роли; prestart roles
читают закреплённый graph, а не изменившийся каталог. Native execution запрещён.

`GET /api/swarms/{id}/eval` для видимого роя возвращает 200 с
`{swarmId, referenceSwarmId, referencePass, definition, model, source, goal,
nonProduction, execution, replay}`. `definition` имеет name/version/revision,
`source` — origin/commit; `replay` — null либо
`{requestId, state, reason, interruptedChildren}`. State — `applied|stopped`,
`interruptedChildren` — boolean, не количество. Неизвестная binding — 404;
прочие ошибки не означают ordinary swarm. Чтение не запускает работу.

`POST /api/swarms/{id}/eval/start` запускает только первый проход bound selection.
Тело отсутствует либо содержит только пустой JSON object/null; selection overrides
и повторные/неизвестные поля — 400. Успех 200 возвращает обычный execution DTO.
Повторный проход и ordinary start bound candidate — 409 `swarm eval conflict`;
ordinary start отказывает до разрешения Git branch. Изменившиеся authority, scope,
source или inventory не превращаются в разрешение запуска.

Recorded node answers применяются только к совпадающим node/options. Исторический
автор остаётся автором исходной записи; applied answer принадлежит launcher, не
имитирует человека. Несовпадение, actor-origin или policy-вопрос сохраняют durable
stop/reason. Live answer API отвергает eval с 409. Прерывание outstanding children
требует отдельной положительной owner authority, а не выводится из replay.

Runtime marker проверяется fail-closed. Для candidate workspace оператор
настраивает отдельные Git credentials проекта и явную network policy; binding
их не выдаёт, сервер не переходит на installation fallback. Гостевая проекция
проектных секретов пуста, основная и малая модели закреплены. Git push, native
workload и actor-origin lifecycle/landing take запрещены; authenticated owner
stop/delete остаются доступны. Эти ограничения не заменяют sandbox
произвольного project code и сетевую политику Substrate.

Operator CLI: `bro swarm eval bind SWARM --reference SWARM --pass N
--definition NAME --model NAME [--json]`, затем `bro swarm eval start SWARM
[--json]`; `bro swarm eval status SWARM [--json]` читает binding/replay. Нет
model defaults, start overrides или automatic retry. Панель показывает отдельную
binding form и pinned start; ordinary start недоступен при bound/unknown/error
marker, а boolean interruption локализован как Yes/No. Trace/compare остаются
самостоятельными read-only представлениями.

## Привязка ролей роя к акторам

Устройство и инварианты — [MODEL.md](../docs/MODEL.md), «Привязка ролей»
(#208, #235). Все три маршрута отвечают представлением ролей:
`{"swarmId", "execution", "graph", "roles"}`. `graph` — `null` или
`pinned` (снимок роя или определение каталога), `name`, `version`,
`revision`. Элемент `roles` по имени роли: `role`, `inGraph`, `state`
(`ready`, `unbound`, `stale`, `inactive`, `foreign`, `missing`) и у
привязанной роли `actorUid`, `actorName` (только у актора этого роя),
`actorIncarnation` — записанная инкарнация.

Managed inventory передаёт `class: persistent|ephemeral` независимо от роли
`master`/`worker`. Панель и `bro swarm roles` не предлагают ephemeral для
обычных ролей графа; отсутствие `class` у legacy-сервера совместимо. Общий
список не скрывает временных исполнителей и не ограничивает ручные действия owner.

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
be provisioning or running and meant to run`, 409 `actor is waiting to stop
for a reset or update; bind it once it runs again` (Stop reset или update ещё
не дошёл до PVE, фаза актора `running`, #275), 409 `actor incarnation
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
  `goal` — только у этого первого шага, как и `sourceCommit` — commit
  источника прохода (#229). До первого запуска полей нет.
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
- Git-идентичность актора (#249): `GET /api/proxmox/workspace/{uid}/{generation}`
  при настроенном `BROOD_ACTOR_GIT_DOMAIN` и непустом origin несёт
  `gitIdentity` `{name: "brood <актор>", email: "<актор>@<домен>"}`; первый
  клон и checkout подтверждения пишут её в `user.name` и `user.email`
  `.git/config` клона, checkout отказывает (`refused` с `detail`) клону с
  другой записанной идентичностью. Без настройки поля нет и поведение
  прежнее (docs/PROXMOX.md, «Origin и первый клон», «Подтверждение
  workspace»).
- Модели Claude Code актора (#263): тот же ответ при заданных
  `BROOD_ACTOR_MODEL` и/или `BROOD_ACTOR_SMALL_MODEL` несёт `models`
  `{model, smallModel}` только с заданными именами, с origin и без; гость
  ставит `model` в `ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_OPUS_MODEL` и
  `ANTHROPIC_DEFAULT_SONNET_MODEL`, `smallModel` — в
  `ANTHROPIC_DEFAULT_HAIKU_MODEL` окружения Claude Code. Без настроек поля
  нет и окружение прежнее (docs/PROXMOX.md, «Модели Claude Code актора»).
- Инструкция — одно сообщение канала роя: `from` — `swarm:<id роя>`, `to` —
  имя привязанного актора, `waitSec` 0. Тело: `[brood swarm <имя>] Step <n>,
  node <узел>.`, блок `Goal of this pass (started at step <первый шаг
  прохода>; earlier passes may have had other goals):` с задачей прохода
  (#242), у шага не первого в проходе — блок `Previous step <n> (node
  <узел>, outcome <исход>):` (#248), блок `Node instruction:` с текстом узла,
  строка с `run_id`, `sequence` и `request_id` для `pipeline_visit_complete`,
  по строке на исход `success` и `fail` с тем, куда он ведёт граф, просьба
  приложить `report` (что сделано и где результат: ветка, MR, документ) и
  подсказка про `ask_operator`. Задача и текст узла обрезаны по краям от
  пробелов. Без блока предыдущего шага не больше 64 КиБ + 4096 байт задачи;
  тело с закрывающим тегом конверта
  сообщения, шаг без задачи прохода или такое тело, чей NDJSON у гостя
  (auth-кадр, конверт, экранированное тело) превысит 128 КиБ, не
  отправляется (`refused`). Ни задача, ни текст узла не граница
  безопасности.
- Блок предыдущего шага (#248) — только шаг непосредственно перед этим в
  том же проходе. После human-узла: `Operator was asked:` (вопрос),
  `Material the operator saw:` (`context` запроса), `Operator chose:
  <подпись> (<исход>)`, `Operator comment:` (текст ответа, если есть); после
  actor-узла: `Report:` с отчётом или `The actor left no report.` Части через
  пустую строку, обрезаны по краям от пробелов, закрывающий тег конверта
  заменён на `[envelope closing tag removed]`. Блок не больше 36 КиБ;
  материал (вопрос, `context`, ответ, отчёт), не влезающий в этот предел или
  в кадры relay и гостя, усекается с конца с пометкой `(truncated, N bytes
  omitted)`, шаг из-за него не отказывается. Заголовок блока и выбор не
  усекаются: если места нет даже им, шаг `refused` с `detail` `the node
  instruction cannot be delivered: the node instruction leaves no room for
  the previous step`. Ни ответ оператора, ни отчёт не граница безопасности.
- С `waitSec` 0 гость ждёт квитанцию доставки Claude Code до 15 секунд
  (#260) и отвечает сразу по ней (`ok: true`, `verdict: "delivered"`) или по
  отказу сессии; без квитанции — `ok: false`, `verdict: "submitted; delivery
  unconfirmed"`. С ожиданием простоя квитанция ждётся в окне `waitSec`, как
  прежде. Каждое звено канала держит его `waitSec` + 15 секунд, а при
  `waitSec` 0 — 30 секунд (docs/PROXMOX.md, «Приём сообщений в госте»).
- Ответ гостя на сообщение канала роя (`brood-swarm-msg`) — `{ok, verdict}`
  и необязательное `notSent` (#244): `true` — отказ до записи в сессию
  Claude, сообщение заведомо не ушло. Инструкцию с таким ответом сервер
  повторяет каждые 3 секунды до 120 секунд, перепроверяя шаг перед каждой
  отправкой, затем шаг `refused` с `detail` `instruction not sent: the actor
  session did not become ready in 120 seconds (...)`. Ответ без поля —
  неизвестный исход, как раньше. `send` оператора (вердикт гостя, исход
  `unconfirmed`) и доставка ответа на запрос роя (`delivery: "failed"`,
  повтор — `/deliver`) такой отказ не повторяют.
- `pipeline_visit_complete` принимается от актора шага той же инкарнации с
  тем же `request_id`, пока он привязан к роли узла и рой идёт; шаг роя с
  источником — только с записанным подтверждением, в том числе из
  `unconfirmed`, если потерялась только квитанция доставки (#245). Иначе гость
  получает `pipeline completion conflict` или `pipeline completion unavailable`.
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

## Профиль Telegram

Устройство и поллер — [MODEL.md](../docs/MODEL.md), «Telegram» (#281).
Маршруты действуют только для самого вызывающего, за
обычными session/CSRF; аноним получает 401, запись перепроверяет сессию в
своей транзакции (403 `session authorization changed`).

- `GET /api/me/telegram` — 200 `{bot_username, own_bot, state, events, chat,
  link_code_pending}` и, когда есть, `reason`, `bot_reason`,
  `link_code_expires_at`. `bot_username` — бот привязки: свой или, без него,
  бот установки (пусто, пока его имя не известно или бота нет); `own_bot` —
  задан ли свой токен; `state` — `unlinked`, `linked` или `error` с `reason`;
  `bot_reason` — причина остановки бота установки; `events` — подписки;
  `chat` — `{telegram_user_id, chat_id, username?}` у `linked`, иначе `null`.
  `chat.username` — имя пользователя Telegram собеседника без `@` из строки
  привязки (#294): из `/start` с кодом, затем из последнего сообщения или
  нажатия кнопки чата, и после перезапуска сервера оно то же. Его нет у
  собеседника без имени и у имени не формы Telegram (латиница, цифры и `_`,
  до 32 знаков).
  Пользователь без привязки получает `unlinked` со всеми подписками.
- `PUT /api/me/telegram` — ровно `{"token"}` строкой; 200 — профиль. Имя бота
  берётся из `getMe`, не из тела. Отказы: 400 тело или форма токена; 422
  Telegram отверг токен (`getMe` ответил 401 или 404) или назвал бота
  негодным именем; 503 `getMe` отказал иначе, например 429 (`Telegram refused
  getMe: try again later`), или Telegram не ответил (сеть, 5xx, 4xx не от Bot
  API, как страница прокси); 409 бот другого пользователя или бот установки.
- `DELETE /api/me/telegram` — снимает свой бот и привязку чата; 200 —
  профиль.
- `POST /api/me/telegram/link` — 201 `{code, expires_at, bot_username, url?}`:
  одноразовый код, `url` — `https://t.me/<bot_username>?start=<code>`, когда
  имя бота известно. Отказы: 409 без бота (нет своего и бота установки), 409
  у пользователя без своего бота, пока бот установки остановлен 401 или 409
  (`the installation telegram bot is stopped: <bot_reason>`), 409, когда свой
  бот стал ботом установки (та же причина, что `reason` привязки), и 409 при
  привязанном чате.
- `PUT /api/me/telegram/events` — ровно `{"events": [...]}` массивом строк,
  полный набор подписок (#295); 200 — профиль. Повтор вида схлопывается,
  порядок `events` в ответе — словаря хранилища (`store.TelegramEventKinds`),
  а не тела; `[]` — ни одной подписки. Отказы: 400 тело не той формы
  (`telegram events request must be exactly {"events"} with an array of
  strings`) и 400 неизвестный вид (`unknown telegram event; known events:
  <словарь через запятую>`): присланное значение отказ не повторяет. Отказ
  ничего не пишет.

Ответ записи бота и привязки приходит после сверки набора поллеров: 200
`PUT /api/me/telegram` — поллер нового бота запущен, 201 `POST /link` —
поллер бота привязки запущен, 200 `DELETE` — поллер снятого бота остановлен.
Подписок поллеры не касаются: `PUT /api/me/telegram/events` отвечает без
сверки. Уход клиента после фиксации записи сверку не отменяет. Запись
зафиксирована, а сверка не прошла: `PUT /api/me/telegram` и `POST /link`
отвечают 500, код не показывается (новый `POST` выдаст другой); `DELETE`
останавливает поллер снятого бота и без сверки и отвечает 200.

Токен бота не возвращает ни один ответ и ни один отказ. Сознательный отказ —
`{"detail", "reason"}` с одной публичной причиной, как у команд (#276);
внутренний сбой — `{"detail": "internal error"}`.

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
