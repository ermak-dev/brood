# GitLab

`go/brood/gitlab` — библиотечные слои #302: ограниченное чтение REST API v4
одного явно привязанного проекта, чистая оценка свидетельств CI и одноразовый
transport создания same-project MR. Зависимости — только Go stdlib. Пакет не
подключён к серверу, гостю, CLI, MCP или хранилищу; сам клиент схему базы и
production не меняет. Наличие клиента не означает
завершение #302.

Отдельный следующий слой `store/gitlab.go` добавляет схему 33: пустые по
умолчанию API bindings/grants, durable intent journal и типизированные одноразовые
merge approvals. Его транзакционные контракты и границы будущего server wiring —
[Go: GitLab authority в хранилище](GO.md#gitlab-authority-в-хранилище-302).
Он хранит только credential reference, не token bytes, и не вызывает client.
Policy отсутствует по умолчанию; generic swarm answer merge не разрешает.
Исполнения merge по-прежнему нет. Store пока допускает только HTTPS origin того
же host без subpath; более широкая форма APIBase клиента это ограничение не снимает.

## Привязка и credential

`NewClient(token, Config{APIBase, ProjectID, Timeout})` принимает API token
отдельным обязательным аргументом. Значения по умолчанию для host и проекта нет.
`APIBase` — фиксированный HTTPS root, заканчивающийся `/api/v4`; допустим простой
путь префикса self-managed GitLab, например `https://git.example/gitlab/api/v4`.
Userinfo, query, fragment, dot-segments, percent-encoded и неоднозначные пути
отвергаются. `ProjectID` — положительный `int64`, не namespace/path проекта.
API base и project binding не меняются после создания клиента.

Вызывающий слой должен передавать доверенную настройку endpoint и отдельный API
credential с минимальным необходимым scope (`read_api` для чтений, `api` для
создания MR). Библиотека не повышает scope автоматически.
Библиотека не проверяет scope токена и не выдаёт grants. Она не читает настройки,
файлы, окружение, Git config или credential helper; Git transport credentials и
разработческий `GITLAB_TOKEN` не являются fallback. Сеть обращается только к явно
заданному origin; это не фильтр приватных IP, DNS rebinding или универсальный
SSRF-шлюз для недоверенных пользовательских URL.

Token уходит только в `PRIVATE-TOKEN` request header. Client не форматирует его
при `fmt`/JSON, DTO его не содержат. Даже отражённое сервером точное значение
токена маскируется в строковых полях DTO после JSON decoding; это не классификатор
произвольных секретов и не детектор любых преобразований/кодировок credential.
Пакет ничего не журналирует. `*Error` хранит только локальный `Kind` и числовой
HTTP `Status`, без тела/заголовков ответа, URL и исходного error chain. Timeout и
отмена различаются через `errors.Is` с `context.DeadlineExceeded`/`context.Canceled`.

## Чтения

Все перечисленные методы принимают `context.Context` и выполняют только `GET`:

| Метод | Endpoint под `/projects/<ProjectID>` |
|---|---|
| `Project` | метаданные проекта |
| `Branch(name)` | `/repository/branches/<URL-encoded name>`; точные name и полный commit ID |
| `Issue(iid)` | `/issues/<iid>` |
| `MergeRequest(iid)` | `/merge_requests/<iid>` |
| `MergeRequestPipelines(iid)` | `/merge_requests/<iid>/pipelines` и проверочные чтения ниже |
| `Pipeline(id)` | `/pipelines/<id>` |
| `Jobs(pipelineID)` | `/pipelines/<pipelineID>/jobs?include_retried=false` и `/pipelines/<pipelineID>/trigger_jobs` (совместимость: `/bridges`) |

Идентификаторы — только положительные числа. Пути составляются из фиксированных
сегментов и десятичных ID, query — через `url.Values.Encode`; ref/branch/title,
`web_url`, `Location` и `Link` не становятся адресом следующего запроса.
Ответ обязан совпасть с запрошенными project/object IDs. MR допускается только
с source, target и собственным `project_id`, равными привязке: fork MRs пока
отвергаются, а не переходят в чужой проект. Job обязан назвать точный project и
pipeline. Пропущенные идентификаторы не заменяются ожидаемыми значениями.

`Jobs` объединяет build jobs и trigger/bridge jobs: обычный `/jobs` не содержит
trigger jobs. [GitLab Jobs API](https://docs.gitlab.com/api/jobs/) называет
`/trigger_jobs` текущим endpoint с GitLab 19.2, а `/bridges` — устаревшим именем.
Upstream `Ci::JobsFinder` по умолчанию выбирает `Ci::Build`, для второго endpoint —
`Ci::Bridge`; оба списка по умолчанию используют `latest`, без retried attempts.
Триггеры декодируются отдельным типом `Bridge`, затем становятся `Job` для оценки.
Их собственный parent pipeline обязан совпасть с project/pipeline binding;
повтор ID между двумя списками тоже вызывает отказ. `downstream_pipeline` не
сохраняется и не обходится: дочерний либо чужой pipeline не расширяет scope.

Совместимость ограничена одним случаем: **404 на первой странице**
`/trigger_jobs` разрешает полное чтение `/bridges` того же pipeline и проекта.
404 не доказывает старую версию: скрытый/недоступный route выглядит так же.
Поэтому только успешное ограниченное и проверенное legacy-чтение разрешает
результат. Ошибка legacy endpoint, 401/403/429/5xx, transport/response errors
либо 404 после первой страницы дают отказ, а не build-only список. Нет fallback
на pipeline status, произвольный endpoint или молчаливого предположения, что
триггеров нет. Этот дополнительный 404-запрос тоже расходует общий page budget.

Сокращённый ответ GitLab списка MR pipelines может не иметь `project_id`.
Поэтому `MergeRequestPipelines` сначала проверяет MR, затем читает список и
каждый pipeline через project-bound `Pipeline` endpoint. Это дополнительные
последовательные чтения в рамках общего срока и byte budget, а не доверие
сокращённому ответу. Чужой проект в списке или при hydration — отказ.

## Границы чтения

- Общий timeout одной операции — 30 секунд по умолчанию; настройка допускает
  положительное значение до 2 минут. Срок caller context может только сократить
  его. Все страницы, build/trigger jobs, проверка MR и hydration делят один срок.
- Один JSON body — не более 4 MiB; сумма bodies операции — не более 16 MiB,
  включая MR и hydration. Для определения превышения читается максимум ещё один
  байт. HTTP headers ограничены 32 KiB, компрессия не запрашивается.
- Страница — по 100 строк, максимум 10 запросов страниц на операцию. Build jobs,
  trigger jobs и compatibility probe делят этот лимит, а не получают по 10 каждый.
  JSON-массив
  декодируется поэлементно: наличие 101-й строки вызывает отказ до её decoding,
  без выделения DTO для остатка массива. Сам body по-прежнему сначала читается
  целиком в пределах byte budgets выше. Валидный last/total разрешает закончить
  даже полную страницу. Без такого доказательства при полной последней
  разрешённой странице клиент отказывает, если понадобилась бы ещё одна:
  ровно 1 000 строк не гарантируют успех.
- Все поддержанные `Link` relations проверяются, не только `next`: origin, путь
  и query обязаны совпасть с локальной привязкой, кроме номера страницы.
  `first` — ровно страница 1, `prev` — текущая минус 1 (не на первой), `next` —
  текущая плюс 1, `last` — не меньше текущей. Несколько relations у одного target
  обязаны одновременно соответствовать ему; повтор relation отвергается.
  `X-Page`, `X-Per-Page`, `X-Prev-Page`, `X-Next-Page`, `X-Total-Pages`, `X-Total`
  сверяются между собой и с Link. Повторные, нечисловые, неканонические значения
  и противоречия дают отказ. Пустые next/prev обозначают границу списка.
  Если `last`/total доказывает непрочитанные страницы без корректного next,
  результат — incomplete response, даже при короткой странице. Ранее полученные
  last/total не забываются на следующей странице; их изменение вызывает отказ.
  Один валидатор хранит размеры всех прочитанных страниц: поздний last/total
  проверяет историю, а не только текущий ответ. При объявленном last каждая
  предшествующая страница обязана содержать 100 строк. Пока известный last
  доказывает непрочитанные страницы, текущий ответ обязан иметь корректный next;
  терминальная страница после первой не может быть пустой. Нулевой total не
  разрешает непустой список; накопленное число строк сверяется с `X-Total`,
  а на терминальной странице обязано совпасть с ним. Без объявленного last
  пустая страница после полной остаётся допустимым проверочным чтением.
  Имена параметра `rel` и зарегистрированных отношений нечувствительны к регистру;
  пробелы и табуляция вокруг `=` допустимы. Поддерживается узкое подмножество
  RFC 8288: единственный параметр `rel` с отношениями `first`, `prev`, `next`,
  `last`, в кавычках или одним token. Повторные параметры, extension-параметры,
  неизвестные отношения и неподдерживаемый quoted syntax отвергаются целиком;
  это строгая политика отказа, не общий parser Link с восстановлением ошибок.
  Чужие/циклические/конфликтующие или неподдерживаемые pagination metadata —
  отказ, а не сетевой переход. Полная страница без доказательства last требует
  проверочного чтения следующей страницы. Короткая страница без next завершает
  список, только если ранее объявленный last/total не доказывает обратное.
  Повтор ID или более 100 строк на странице отвергает весь список.
- Redirects, включая same-origin, не выполняются. Нет environment proxy,
  cookie jar, автоматического retry после ошибки или скачивания traces,
  artifacts и URL из ответа. Стандартный HTTP transport может повторить безопасный
  GET при потере ранее использованного idle connection.
- При любой ошибке списки возвращают `nil`, одиночные DTO — нулевое значение:
  усечённый JSON, оборванный body, поздний сбой страницы/hydration или превышение
  лимита не превращаются в частичные положительные свидетельства.

Offset pagination не является атомарным snapshot: дубликаты при сдвиге
обнаруживаются, но произвольное одновременное изменение набора jobs/pipelines не
доказуемо исключить. Между раздельными чтениями pipeline и jobs статус может
измениться. Библиотека не обещает CAS или свежесть MR head.

## Свидетельства CI

`AssessCI(Pipeline, []Job) CIEvidence` не делает I/O и не меняет аргументы.
Свидетельство сохраняет project ID, pipeline ID и SHA вместе с verdict. Основа —
`is_failure` и pipeline diagnostics из `brood/dev/gitlab.py`:

- Любая build или trigger/bridge job `failed` без `allow_failure` даёт
  `failed/jobs_failed`, даже пока pipeline ещё `running`. Исключения по stage нет,
  включая deploy. ID проваленного триггера сохраняется в `FailedJobIDs`.
- Терпимые провалы идут отдельно в `AllowedFailureJobIDs` и не названы причиной.
- Статус pipeline `failed` или `yaml_errors` даёт `failed/pipeline_failed`, в том
  числе при отсутствии jobs. `yaml_errors`, `failure_reason` и
  `detailed_status.text` сохраняются отдельными полями, не парсятся как prose.
- `success/complete` возможен только для успешного pipeline с непустым набором
  согласованных current jobs; каждая — `success` или терпимый `failed`.
  Project, pipeline и SHA jobs должны совпасть; повтор job ID блокирует оценку.
- Идущий pipeline/job даёт `waiting`. Неизвестные, manual, skipped, canceled и
  прочие незавершённые состояния не дают успеха. Неполные либо несогласованные
  идентификаторы дают `blocked` со структурированным reason.

Набор jobs вызывающий слой должен получить полным чтением `Jobs`, без фильтрации
и обрезки: чистая функция не может доказать полноту произвольного slice. Даже
`success` — **не merge authority**: нет проверки актуального MR head, approvals,
mergeability, lifecycle authority или grants. Кроме узкого создания MR ниже,
нет методов merge/retry/cancel, комментариев или изменения issues/MR. Их появление
и хранение API credentials требуют отдельных слоёв и review, не следуют из клиента.

## Создание MR

`CreateMergeRequest(ctx, allowedTarget, CreateMergeRequestInput)` выполняет один
`POST /projects/<ProjectID>/merge_requests`. `allowedTarget` — точная ветка из
доверенной политики вызывающего слоя, не копия недоверенного аргумента запроса.
`TargetBranch` обязан совпасть с ней, `SourceBranch` — отличаться. Project ID,
endpoint и token берутся только из созданного клиента; override и fork-проектов
в аргументах нет. Transport не проверяет пользователя, grants, инкарнацию актора
или право создания MR: эти проверки обязательны в вызывающем серверном слое.
Caller также обязан ограничить source веткой, разрешённой конкретному актору и
инкарнации (например, его branch prefix), а не разрешать любую ветку проекта.

Входы и ответ ограничены локально:

- Source/target — от 1 до 255 ASCII-байт, только буквы, цифры и `/._-`.
  Не допускаются `HEAD`, полный `refs/…`, начальный `-`, пустые компоненты,
  `..`, компоненты с начальной/конечной точкой или суффиксом `.lock`, а также
  имена из ровно 40 hex-символов (ограничение GitLab, чтобы не путать с commit).
  Это намеренно уже `git check-ref-format --branch`, не общий parser Git refs.
- `SourceSHA` — ожидаемый полный lowercase hex SHA длиной 40 или 64 символа.
  Ответ обязан вернуть тот же SHA. Это **не CAS**: create API не принимает SHA
  precondition. При гонке MR мог создаться с другим head; результат uncertain.
- `Title` — непустой UTF-8 до 255 байт, без управляющих символов/переводов строки.
  `Description` — явно переданный UTF-8 до 8 KiB, может быть пустым; разрешены
  tab/CR/LF, остальные управляющие символы запрещены. Лимиты намеренно строже
  [GitLab create API](https://docs.gitlab.com/api/merge_requests/#create-mr),
  который допускает description до 1 048 576 символов. Предел 8 KiB оставляет
  место всем аргументам в canonical journal payload до 64 KiB даже при шестибайтном
  JSON escaping; caller всё равно проверяет размер своего полного envelope.
- Строка description, начинающаяся после whitespace с `/`, запрещена, включая
  CR-разделители: GitLab quick actions могут менять MR и полномочия, их нельзя
  прятать в тексте create. Даже безобидный slash-текст требует переформулировки.
- `RequestID` — обязательные 1–64 ASCII-байта из букв, цифр, `-` и `_`, без
  секретов. Он передаётся в `X-Request-ID` и в добавленном к description маркере
  `<!-- brood-mr-create:<RequestID> -->`. Входной description не может содержать
  этот префикс. Маркер — помощь ручной сверке, не защищённое свидетельство и
  **не idempotency key**; заголовки idempotency не устанавливаются. Caller
  генерирует ID на сервере до резервирования intent (например, новый ULID),
  обеспечивает уникальность во всём bound проекте между акторами/инкарнациями и
  включает ID в canonical payload. Actor-local счётчик или `GitLabOperation.RequestID`
  сам по себе не гарантирует эту уникальность: store ограничивает его иначе.
- Timeout — общий клиентский (30 секунд по умолчанию, до 2 минут) с сокращением
  caller context; response body — до 4 MiB плюс один проверочный байт, headers —
  до 32 KiB. Успех — только HTTP 201 и полный typed MR с положительными ID/IID,
  всеми тремя project IDs из привязки, точными source/target/SHA и state `opened`.
  Иначе DTO нулевой. Отражённый token маскируется как при чтениях.

Перед вызовом caller обязан атомарно зарезервировать durable intent, связать его
с `RequestID` и неизменяемыми аргументами и **зафиксировать `sending` в базе до
сети**. Journal хранит hash canonical payload, не body: caller обязан отдельно
обеспечить durable связь intent с точными аргументами и correlation ID, а не
генерировать новый ID при повторном обращении актора. Сам transport базы не
касается. Сбой caller после отправки, даже при потере успешного ответа до записи
результата, требует того же reconciliation.
Нельзя вызывать повторно только потому, что в базе не успели сохранить MR IID.
При записи исхода caller использует точное соответствие состояниям journal:
успешный MR и `nil` error → `completed`; `CreateNotSent` → `refused`;
**любой `CreateUncertain` → `unknown`**, независимо от `Kind` и HTTP status.
Нельзя превращать 409/422 или иной post-send отказ в `refused` по коду HTTP.

`*CreateError` хранит только локальные `Kind`, числовой `Status` и `Effect`:

- `not_sent` доказывает только локальный отказ до `http.Client.Do`: невалидный
  ввод или уже отменённый/истёкший context. Сеть не вызывалась.
- `uncertain` возвращается при любой ошибке после передачи в HTTP client:
  потеря соединения, timeout/cancel, TLS/dial, любой не-201 status, усечённый или
  слишком большой body, некорректный JSON, чужой проект/ветки/SHA. Даже 4xx не
  объявляется доказательством отсутствия эффекта: upstream create contract не
  обещает общего rollback для всех таких ответов. Status объясняет наблюдение,
  не разрешает replay. Исходный error chain, URL, body и headers не сохраняются;
  `errors.Is` по-прежнему различает cancel/deadline.

POST не повторяется: нет цикла retry, redirects или fallback; `GetBody` очищен
при непустом body, чтобы stdlib transport не повторил POST после zero-byte write
на reused connection. Для uncertain caller блокирует автоматический повтор и
проводит ручную сверку intent/маркера/веток/SHA в GitLab. Эта библиотека не ищет
существующие MR автоматически и не доказывает уникальность по маркеру. При
ручной сверке нужен точный полный финальный marker line, а не case-insensitive
или substring-поиск, плюс сверка project/веток/SHA. Другие варианты написания
маркера в произвольном тексте не считаются совпадением; сам маркер не является
доказательством авторства или права на MR.

Создание MR **не merge approval**, не доказательство свежего head/успешного CI
и не право включать auto-merge. Merge endpoint, изменение approvals/grants,
произвольные поля GitLab API и generic proxy отсутствуют. Существующие read-only
поведение и бюджеты не меняются. Серверный write wiring и live приёмка остаются
отдельными слоями #302; этот transport задачу не закрывает.

## Проверки

`go test ./brood/gitlab` использует собственные `httptest` TLS servers, без live
GitLab. Проверены typed endpoints, IDs и fork refusal, все pagination/Link
relations и смешанные X headers, failed bridge при успешных builds, недоступный
trigger endpoint, узкий legacy fallback и его page budget, truncation, response
и общий build/trigger byte/page/time budget, redirects, nil-on-error и
sentinel leakage через DTO, `fmt`, JSON и error traversal. Отдельно — чистые
табличные CI verdicts. Для create проверены local refusal без сети, точные поля
POST и correlation metadata, все классы HTTP отказов без replay, потеря ответа,
zero-byte write на reused connection, caller cancel/deadline, malformed/truncated/
oversized 201, чужие project/branch/SHA и redaction через fmt/JSON/unwrap. Это
библиотечное доказательство, не приёмка будущей интеграции #302 и не разрешение
merge или deploy.
