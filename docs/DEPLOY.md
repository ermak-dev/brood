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

Preflight проверяет настройки, свободное место раздела Docker (если задан
`BROOD_DEPLOY_MIN_FREE_GB`, раздел «Место на машине деплоя»), TLS, владение
установкой/namespace, approved worker images, точные Kubernetes API endpoints
и identities существующих server/edge ресурсов до сборки и записи. Namespace
должен уже принадлежать установке; новый Substrate подготавливается отдельной
операцией.

Полная выкладка:

0. Проверяет свободное место раздела Docker до Lease, сборки и любой записи.
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
7. После успеха убирает за собой, если заданы настройки: старые снимки базы
   (`existing`) и старые теги своих образов с кэшем сборки Docker (раздел
   «Место на машине деплоя»).

`bin/brood deploy go` остаётся подготовкой только приватного Go endpoint,
без public edge, bootstrap пользователя или очистки старых данных.

## Существующая Go-установка

`BROOD_DEPLOY_MODE=existing` — явный opt-in в приватном `.env` релиза.
Без настройки действует прежний `initial`: bootstrap и обязательный login gate.
Неизвестный режим — отказ. В `existing` admin username/password не требуются;
пустой `BROOD_ADMIN_PASSWORD` допустим. Пароль soleadmin не сбрасывается,
`brood-ci-check` не создаётся. Operator password/cookie не передаются в CI.
Изменение protected `.env` выполняет оператор отдельно, не deploy-код.

До записи требуются принадлежащие `brood-go` Deployment/Service/PVC и оба
публичных edge-ресурса, bound PV с точным claim UID, единственный готовый
server pod с цепочкой Pod → ReplicaSet → текущий Deployment. Отсутствие
установки, чужой ресурс, незавершённый rollout или неподдерживаемый DB layout —
отказ, не переход к initial install. Source path читается из явного
`BROOD_DB_PATH` текущего Deployment и сверяется с runtime pod, data mount и PVC.
Сейчас допускается штатный `/data/brood.db`: иной layout отвергается, поскольку
новый rollout не должен переключаться на новую пустую БД.

После сборки нового Go-образа, но **до первого apply сервера и миграции**, gate
`brood/dev/deploy/existing_install.py` запускает одноразовый bounded Pod:

- image — точный digest нового build `localhost:5002/brood-go@sha256:…`;
- `/brood database snapshot --path /data/brood.db --output /data/.brood-snapshot-<random>.db`;
- только существующий owned PVC, на node текущего server pod (node affinity),
  без service-account token, Secret mounts, shell, host namespace и сети;
- отдельная deny-all NetworkPolicy создаётся до Pod; non-root UID 65532,
  read-only rootfs, dropped capabilities, seccomp, CPU/memory limits,
  deadline 180 секунд и bounded wait;
- helper обязан вернуть только JSON metadata: `schema_version`, `enabled_admins`,
  `users`, `sha256`, `bytes`. Некорректный ответ или отсутствие enabled admin — отказ.

Snapshot CLI использует согласованный SQLite backup с WAL, без миграции;
это не копирование одного main DB файла. Результат 0600 остаётся на приватном
PVC, не попадает в release archive, stdout или CI artifacts. В CI виден только
путь и итог gate, не содержимое базы. Helper Pod и policy удаляются только со
своими UID preconditions; policy сохраняется, если удаление Pod не подтверждено.
При admission-изменении volumes, scheduling или security gate отказывает.
Если helper невозможно безопасно запустить или snapshot не подтверждён,
серверный rollout не начинается. Вручную пропустить backup нельзя.

### Schema gate: новый образ обязан открыть или мигрировать snapshot (#220, #274)

Сразу после snapshot и **до apply** второй одноразовый helper с теми же
ограничениями (тот же новый digest, deny-all сеть, PVC только read-only)
выполняет `/brood database verify --path <snapshot> --sha256 <hash>`, где hash
взят из метаданных только что сделанного snapshot. Verify идёт тем же кодом,
которым `store.Open` нового сервера примет, мигрирует или отвергнет базу при
старте ([GO](GO.md), «Миграции схемы»): `user_version` равен версии схемы
образа или ведёт к ней цепочкой миграций образа, и тогда шаги применяются к
копии байтов в памяти; затем определения таблиц, индексов и триггеров всех
модулей схемы совпадают, owner grant у каждого роя и счётчики ревизий workflow
согласованы. Snapshot на PVC не пишется и не мигрирует (подробности чтения - в
разделе recovery ниже). Ответ helper - JSON `schema_version` (версия snapshot),
`target_version` (версия, до которой сервер доведёт базу) и `verified`; цель
ниже версии snapshot - нечестный ответ и отказ. Gate действует в **каждом**
`existing` запуске, а не только в recovery, и не зависит от
`BROOD_DEPLOY_SCALE_FENCE`: штатный готовый сервер для него не гасится.

Релиз, мигрирующий базу (N < M с цепочкой N→M), проходит gate. Миграцию
выполняет сам сервер при старте после apply; стратегия Deployment `Recreate`
гасит прежний pod до старта нового, так что писатель один. Inspect после
rollout требует `schema_version` равной `target_version` verify; иначе отказ
с Lease, оставленной до решения оператора (исход apply уже не откатывается
автоматически). Итог выкладки называет обе версии:

```
Private database snapshot retained on owned PVC: /data/.brood-snapshot-<id>.db
Database schema migrated from 26 to 27 on server start; rollback restores the
snapshot above with the previous image (docs/DEPLOY.md)
```

Без изменения схемы строка - `Database schema 27: no migration`.

Отказ до apply - база новее образа (старый образ на новой базе, N > M), версия
без цепочки, разрыв цепочки, упавший в памяти шаг или схема, не совпавшая с
реестром. Deployment `brood-go` не записывается, работающий pod остаётся на
прежнем образе и прежней базе, apply edge и probe не выполняются, оба helper
удаляются по UID, Lease снимается, так что повтор с совместимым релизом не
заблокирован. Причина называет объект схемы или версию и не содержит строк
базы; причина SQLite после `failed` не печатается:

```
owned database helper verify failed, reason=Error, exit_code=1: incompatible
database schema: found 27, want 26; the database is newer than this release;
the release image cannot open or migrate the current database, so the rollout
is refused before apply and deployment/brood-go keeps its current image; a
schema change ships with a migration from the deployed version (docs/DEPLOY.md)
```

Остальные причины: `found 25, want 27; no migration from version 25`,
`migration from version 26 to 27 failed`, `table swarms differs from current
schema`, `missing index ...` и проверки данных ACL и workflow.

Правило (#274, решение оператора 2026-11-12): изменение схемы приходит с
миграцией от развёрнутой версии, production ради изменения схемы не
пересоздаётся. Так выкладка #206, изменившая DDL `swarms` без смены версии
схемы, остановилась бы здесь, а не увела бы сервер в CrashLoopBackOff; релиз
без миграции от версии production останавливается так же. Пересоздание базы
или установки теряет данные (как решение #219) и остаётся отдельным решением
оператора для исключительных случаев: deploy его не выполняет, не предлагает
флагом и не делает в ответ на отказ gate. Snapshot отказавшего запуска
остаётся на PVC, как и после любого другого отказа; сам отказ ничего не
удаляет. С `BROOD_DEPLOY_KEEP_SNAPSHOTS` его уберёт хранение снимков
следующей успешной выкладки (раздел «Место на машине деплоя»).

### Откат после миграции (#274)

Snapshot перед apply (#247) - точка отката: он снят до того, как новый сервер
мигрировал базу, и проверен verify. Прежний образ на мигрированной базе не
стартует: образы до #274 пишут `found 27, want 26; recreate the database`,
образы с #274 - `found N, want M; the database is newer than this release`.
Его gate откажет до apply, поэтому откат - это возврат прежнего образа
**вместе** с базой из snapshot, а не один `rollout undo`. Процедуру выполняет
оператор, deploy её не делает:

1. Остановить запись: `kubectl -n brood scale deployment/brood-go --replicas=0`
   и дождаться исчезновения pod с томом `brood-go-data`. Всё, что сервер
   записал после snapshot (ответы, шаги, акторы), откат теряет: сверьте это
   до решения.
2. Одноразовым pod с этим PVC (образ сервера - scratch без shell, поэтому
   отдельный образ с shell; как у helper - без сети и токенов) отложить текущую
   базу и её `-wal`/`-shm` в сторону, например `/data/brood.db.migrated-<время>`,
   и скопировать snapshot этой выкладки в `/data/brood.db` с правами 0600
   владельца 65532. Путь snapshot называет выкладка: при успехе - строка
   `Private database snapshot retained`, при отказе после apply - сам отказ
   (`database snapshot taken before apply: /data/.brood-snapshot-<id>.db`).
   Других признаков у файла нет: имена случайны, на PVC лежат и снимки
   прошлых выкладок, и снимки отказавших попыток. Без пути из вывода берите
   самый новый по mtime и до копирования проверьте его версию тем же
   `brood database verify` прежнего образа: более старый snapshot той же
   версии прежний образ примет молча и потеряет всё, записанное между
   снимками. Snapshot - самостоятельный файл без `-wal`/`-shm`, копируется
   только он.
3. Вернуть прежний образ тем же Deployment: `kubectl -n brood set image
   deployment/brood-go brood=<digest прежнего релиза>` (строка `Go server
   deployed:` его выкладки; образ должен оставаться в registry кластера), затем
   `kubectl -n brood scale deployment/brood-go --replicas=1`. Штатная выкладка
   здесь не годится: `existing` требует готовый pod. Прежний `store.Open`
   увидит версию snapshot (26) и примет её.
4. Проверить `brood database inspect` (счётчики users/admins как в snapshot),
   `bro whoami` и панель. Отложенную мигрированную базу удалить только после
   этого. Следующий релиз - revert поверх main (как в разделе recovery), его
   gate проверит образ на восстановленной базе. Отказ после apply оставляет
   Lease `brood-go-deploy` (неоднозначный исход): до следующей выкладки его
   снимают по процедуре «Ручное снятие Lease» (раздел «Восстановление
   прерванного rollout»), доказав, что держатель мёртв.

Срок жизни точки отката. С `BROOD_DEPLOY_KEEP_SNAPSHOTS=N` хранение каждой
следующей успешной выкладки оставляет N самых новых снимков, и снимки
отказавших попыток занимают те же места: после N выкладок snapshot до
миграции удалён, и откат через него невозможен. Решайте об откате до
следующих выкладок или скопируйте snapshot с PVC заранее.

Границы gate. Verify читает snapshot целиком в память, не больше 32 MiB при
лимите helper 256Mi: база крупнее - отказ `snapshot is too large to verify in
memory` до apply (fail-closed), предел меняется отдельным изменением. Gate
доказывает только то, что проверяет `store.Open`; ошибку старта сервера по
другой причине (настройки, сеть, код после открытия базы) он не ловит.
Миграцию в памяти verify выполняет с `trusted_schema=OFF`: шаг, чья запись
через триггер или индекс зовёт функцию без пометки innocuous, откажет в gate,
хотя прошёл бы в `Open` (fail-closed). `--check` схему не проверяет: образа ещё
нет, gate работает только в полном запуске. Пустой отказ helper без совпадения с грамматикой схемы тоже
останавливает выкладку до apply, но без названия причины.

После готовности нового runtime проверяется exact image digest, затем новый
одноразовый helper выполняет `/brood database inspect --path /data/brood.db`
с read-only PVC mount. Сохраняются количество users и enabled admins, требуется
`enabled_admins >= 1`, а `schema_version` равна `target_version` verify
(#274). Это проверка metadata, **не доказательство входа** и не
проверка пароля. В этом режиме нет вызовов bootstrap, login, createUser и нет
legacy cleanup. TLS/hostname/healthz/React проверки остаются. Вывод разделяет
`Rollout passed` и `operator-authenticated acceptance pending`. Оператор
завершает acceptance своим приватным `bro whoami` и браузером вне CI.

### Восстановление прерванного rollout (#220)

Новый релиз может не открыть БД существующей установки (`table ... differs
from current schema`), и его pod уйдёт в CrashLoopBackOff. Schema gate выше
останавливает такой релиз до apply; recovery нужен установке, которая уже
оказалась в этом состоянии (выкладка до gate, как #206 9 ноября). Штатный gate
требует готовый runtime и без исключения заблокировал бы релиз с
восстановленным совместимым кодом. Поэтому `existing` принимает **не-готовый**
runtime только как recovery, когда доказано всё сразу; иначе отказ прежним
текстом, без дальнейших действий:

- ровно один pod `app=brood-go` (включая завершающиеся), владелец и labels
  deploy, цепочка Pod → ReplicaSet → Deployment по UID, ни один другой
  ReplicaSet не держит реплик, revision ReplicaSet равна revision Deployment,
  `observedGeneration` актуален;
- digest образа Deployment, pod spec и `imageID` совпадают, PVC/PV и путь
  `/data/brood.db` проходят те же проверки, что и обычно;
- `CrashLoopBackOff`, `restartCount >= 1`, последний выход `Error` с кодом 1,
  Ready=false, нет available/ready реплик;
- предыдущий лог содержит ошибку несовместимости схемы. Одна строка лога
  не доказательство безопасности, а лишь необходимое условие: OOM, panic,
  ошибки конфигурации и нечитаемый лог - отказ (лог не печатается).

Дальше цепочка идёт как обычно, плюс обязательные шаги. Snapshot живого PVC
не доказывает отсутствия писателя: CrashLoop pod перезапускается, том могут
монтировать другие pod. Поэтому в recovery **до snapshot** писатель гасится:

1. Список всех pod namespace (не только `app=brood-go`): том `brood-go-data`
   монтирует ровно один не завершённый pod, UID которого доказан. Любой другой
   (Pending, terminating, чужой владелец) - отказ до записи. Ошибка `get pods`
   или нечитаемый ответ - тоже отказ.
2. Протокол scale-to-zero (durable, повторно входимый). Каждый шаг - один
   `kubectl patch deployment brood-go --type=merge` с `metadata.uid` и
   `metadata.resourceVersion` в теле (CAS), ответ проверяется (тот же uid), а
   решение принимается по **повторному чтению** объекта, а не по ответу:
   - проба: два `--dry-run=server` patch (устаревший RV; чужой UID) обязаны
     отказать. Устаревший RV: `Error from server (Conflict): ... the object has
     been modified ...`. Чужой UID (реальный вывод kubectl 1.36, **без**
     `Error from server`): ровно одна строка `The Deployment "brood-go" is
     invalid: metadata.uid: Invalid value: "00000000-0000-0000-0000-000000000000":
     field is immutable` (предвычисленный чужой UID и настоящее имя, допустим
     один завершающий перевод строки). Любой иной текст - webhook, другой UID
     или имя, префикс, приписка - проба не пройдена. Это сравнение строки
     stderr, а не машинный Status: оно не отличает webhook, который напечатает
     тот же текст, поэтому полной гарантии не даёт, только необходимое условие
     (достаточное - сценарий live-verified ниже). Если API patch принял или ответил
     иначе - recovery заблокирован, ничего не изменено. Проба ничего не пишет;
   - намерение: аннотация `brood.dev/quiesce` (JSON: `v`, `id` - случайный
     токен запуска, `uid` Deployment, `phase` - `intent` или `scaled`, `image`,
     `pod` - UID писателя, `spec` - sha256 канонического spec с replicas=0,
     `fence`, `at`; `at` - конечное число, NaN/Infinity/1e999 и время из далёкого
     будущего запись делают нечитаемой). Намерение записывается **до** replicas
     отдельным CAS и подтверждается чтением. Generation в записи нет, он
     сохраняется в процессе (см. ниже);
   - scale: `spec.replicas=0` CAS-patch от resourceVersion подтверждённого
     намерения, тем же patch запись получает `phase=scaled`; успех = прочитанные
     replicas=0, `phase=scaled`, digest spec совпал **и generation равен
     ожидаемому**: generation, прочитанный перед этим CAS, плюс 1 (apiserver
     поднимает его один раз на update, меняющий spec или аннотации). Ответ patch
     не заменяет чтение, а лишь должен с ним согласоваться (новый RV, наша
     запись, тот же generation). При потерянном ответе доказательство - то же
     правило `+1` по чтению; чужая запись между нашей и чтением даёт `+2` и
     блок. Прочитанный generation сохраняется и сверяется в **каждом** fence
     (перед snapshot, при helper, после snapshot, перед verify и apply), так что
     digest spec ловит только итоговое содержимое, а generation - ещё и
     A->B->A, scale 1->0 и чужую аннотацию между guard. После подтверждённого
     scale наших записей в Deployment нет (продление аренды идёт в отдельном
     Lease), поэтому ожидание не обновляется: любое движение generation - блок.
     RV намеренно не сравнивается: его двигает status контроллера;
   - неизвестный исход (потерянный ответ, таймаут): повторное чтение. Применился
     - идём дальше. Не применился - **fence-запись** (`fence`+1, CAS) меняет
     resourceVersion; запоздавший scale со старым RV после неё получает
     конфликт и применён быть не может. Fence засчитан только если чтение
     подтвердило новую запись или иное состояние; неподтверждённый fence - отказ
     без продолжения. Если запоздавший успел раньше, fence
     получит конфликт, а следующее чтение покажет scaled. Конфликт из-за
     status-обновления контроллера - перечитать и доказать всё заново, не более
     8 кругов, иначе отказ и release.
3. Ожидание исчезновения именно этого Pod UID (120 с), затем повторный список:
   ни одного pod с томом. Identity Deployment (uid, spec целиком,
   наша запись с `phase=scaled`; generation не доказательство), Service, PVC, PV сверяются на каждом этапе: перед snapshot,
   при работе helper (том держит только он), после snapshot, после verify.
4. Любой сбой (scale, helper, смена identity, чужой pod, неудавшийся apply)
   разбирает `settle` по записи в объекте, не по памяти процесса: состояние
   `scaled` (uid, `phase=scaled` и digest spec совпали) - одним CAS
   `replicas=1` и снятие записи; replicas уже не 0 (apply или оператор вернули)
   - снимается только запись; replicas=0, но spec или phase не те -
   **ничего не трогаем**, отказ с процедурой оператора. Чужие правки spec
   не перезаписываются никогда: merge-patch меняет только replicas и нашу
   аннотацию. Повторный запуск после release снова проходит через
   CrashLoop-доказательства.
5. Штатный готовый сервер не гасится: консистентный backup и тот же schema
   gate, без scale-to-zero и fence.

Повторный вход после обрыва (SIGKILL, потеря runner, таймаут CI). Принудительная
команда зовёт `--check`, затем `run`. `--check` по-прежнему ничего не пишет: запись
оборванного deploy он только оценивает (те же отказы, что ниже: не разбирается, чужой
UID, моложе 240 с, foreign, существующая Lease) и при годной записи без Lease пропускает как `recoverable`, не проверяя
остальную установку (replicas=0 её бы провалил). Разбирает запись только `run`, и только если Lease нет (после ручного снятия): он
читает аннотацию `brood.dev/quiesce` перед preflight:

- запись не разбирается, не принадлежит этому Deployment UID - отказ;
- запись моложе 240 с - отказ (возможен запрос в пути). Это только нижняя
  граница, а не доказательство, что запуск мёртв: живой запуск идёт дольше
  (helper 190 с на snapshot и столько же на verify, ожидание pod 120 с, круги
  quiesce; худший случай - десятки минут). Живость не решает ни время, ни аренда:
  существующая Lease - отказ, снимает её только оператор (раздел ниже);
- `scaled` - replicas=1 и снятие записи; затем **намеренный отказ** «rerun»:
  pod новый, CrashLoop-доказательства (`restartCount >= 1`) нужно собрать заново;
- intent/idle - снимается запись, выкладка идёт дальше;
- replicas=0 без совпадения digest spec и `phase=scaled` - отказ без записей.
  Известное ограничение: оператор, сам выставивший replicas=0 при нетронутом spec
  до снятия нашей записи, неотличим от нашего scale (generation это не различает);
  такое состояние будет восстановлено до replicas=1.

Сериализация записи: два разных механизма, оба fail-closed. Любой путь записи
(healthy, recovery, разбор оборванной записи) начинается с `ExistingInstall.claim()`,
который `bin/brood deploy server` зовёт до preflight и сборки, и до первой записи
в кластер:

1. **flock (process lock).** Неблокирующий `flock(LOCK_EX|LOCK_NB)` на
   `$XDG_DATA_HOME/brood/deploy.lock` (по умолчанию `~/.local/share/brood/deploy.lock`).
   Он берётся самим Python-кодом, поэтому прямой `bin/brood deploy server` его не
   обходит; принудительная команда `deploy/brood-deploy.sh` держит ещё свой
   `flock` на `releases/.lock`, они независимы. Занят - отказ, не ожидание. Ядро
   снимает flock со смертью процесса (SIGKILL тоже), поэтому он доказывает ровно
   одно: **на этой машине под этим пользователем** другого живого процесса deploy
   нет. Дочерние `kubectl` flock не наследуют (`O_CLOEXEC`): убитый процесс
   мог оставить в пути `kubectl`-запрос, flock его не покрывает.
2. **Lease `brood-go-deploy`** (namespace `brood`, метки deploy). Её создаёт тот же
   `claim()`, и она остаётся до конца **всех** эффектов: apply, inspect, apply edge,
   probe и хранение снимков (#247). Снимает её только `finish()` после успеха или
   `release()` при отказе до `before_apply`. Продление (каждые 20 с и на каждом guard) нужно лишь чтобы
   увидеть ручную подмену, но не как доказательство жизни.

**Автоматического перехвата нет, ни по возрасту, ни по «не менялась N секунд».**
Процесс A может зависнуть после последнего guard и позже выполнить нефенсированный
`kubectl apply` (`replicas=1`) поверх snapshot процесса B; heartbeat не делает
«guard -> apply» атомарным, а время не доказывает смерть. Поэтому любая существующая
Lease (чужая, старая, своего прошлого запуска, без меток) - отказ **до единственной
записи**, в `--check` и в `run`. Следствия, о которых надо знать:

- после SIGKILL, потери runner или таймаута CI следующие `--check` и `run` отказывают,
  пока Lease не снята вручную (процедура ниже); автоматическое восстановление
  оборванного scale-to-zero **не работает**, пока оператор не доказал смерть прежнего
  процесса;
- ошибка после возврата `before_apply` (apply, rollout, публикация артефактов
  гостя, inspect, edge, probe, сбой helper хранения снимков) оставляет
  Lease: исход apply неоднозначен, повтор не делается. После неё `release()` только
  возвращает replicas по записи, если может доказать её;
- `flock` не заменяет Lease: он не виден с других машин, а Lease не заменяет `flock`:
  она создаётся уже после него;
- исключение одно: отказ самого `database prune` (helper завершился Failed или
  ответил не про снимок этой выкладки) - только предупреждение, выкладка успешна,
  `finish()` снимает Lease (раздел «Место на машине деплоя»).

Провал самого удаления Lease (`finish()`/`release()`) - ошибка deploy, не успех: Lease
остаётся, действуйте по процедуре ниже.

Ручное вмешательство (снятие Lease) - единственный способ продолжить, и оно за
оператором; код его не выполняет.

Ручное снятие Lease. Только после доказанной смерти прежнего процесса:

1. Найдите процесс и убедитесь, что его нет ни на машине деплоя, ни на другом runner
   (`pgrep -af 'brood deploy server'`, нет идущего pipeline, нет висящего `kubectl`
   с `apply`/`patch`). Возраст Lease и её `renewTime` этого не доказывают. Кто держал
   Lease, записано в её `metadata.annotations`: `brood.dev/hostname`, `brood.dev/pid`,
   `brood.dev/started` и (если задан) `brood.dev/release` - SHA релиза
   (`BROOD_RELEASE_SHA`). Проверьте на этой машине, что процесса с таким pid нет, а
   `leaseDurationSeconds` (120) информативно и сама Lease не истекает.
   Перед повтором проверьте, не прошёл ли неоднозначный apply/restore: образ и
   `replicas` у `deployment/brood-go`, наличие `brood.dev/quiesce`, rollout status.
2. `kubectl -n brood get lease brood-go-deploy -o yaml`: запишите `metadata.uid`,
   `metadata.resourceVersion`, `spec.holderIdentity`.
3. Удалите её с предусловиями, чтобы не снять чужую, обновлённую за это время:
   `kubectl -n brood delete --raw /apis/coordination.k8s.io/v1/namespaces/brood/leases/brood-go-deploy -f opts.json`,
   где `opts.json` - `{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"<uid>","resourceVersion":"<rv>"}}`.
   Конфликт значит, что Lease жива: остановитесь.
4. Если Deployment `brood-go` несёт `brood.dev/quiesce`, пройдите следующий раздел;
   иначе запускайте deploy заново.

Блокировка до живой проверки. `BROOD_DEPLOY_SCALE_FENCE=live-verified` в
защищённом `.env` - единственный способ включить scale-to-zero; без него
recovery-путь отказывает «blocked» до любой записи, а штатный healthy путь не
затронут (его schema gate работает и без флага). Значение ставит оператор после прохода сценария на изолированном
тестовом кластере: (1) `--dry-run=server` patch со старым RV и с чужим UID
отказывает конфликтом; (2) запись аннотации + scale с верным RV применяются,
со старым RV после любой записи - конфликт; (3) убитый между шагами запуск
разбирается повторным deploy. (4) шаг scale поднимает generation ровно на 1 (правило `+1` выше) и Lease create/patch/delete
проходят с учётными данными deploy; (5) пауза процесса (SIGSTOP) на любое время не даёт второму процессу взять Lease: deploy отказывает при любой существующей. В разработке
сценарий прошёл только на одноразовом kube-apiserver 1.36.2 + etcd 3.6.8 (envtest,
без kubelet и controller-manager, роль контроллеров эмулировал скрипт; отчёт в
#220). Production Kubernetes 1.37 с kubelet, controller-manager, admission и
учётными данными deploy сценарий **не проходил**: корректность там держится на
модели API в тестах и на runtime-пробе, которая отказывает, если API не применяет
предусловия. Признать protocol production-ready нельзя, пока сценарий не пройден
там же, где будет выставлен флаг.

Snapshot (`mode=ro`, VACUUM INTO) -> fence -> helper
`brood database verify --path <snapshot> --sha256 <hash>` -> fence -> apply.
Это тот же schema gate, что на штатном пути, с fence вокруг него. Без verify
apply не происходит: ошибка в `before_apply` останавливает выкладку.

Что перечень pod **не** доказывает. Список pod namespace с claim
`brood-go-data` не видит: pod, монтирующий том через `hostPath` или напрямую
на node (процесс на node, kubelet, CSI-операции); контейнер, принудительно
удалённый (`--force --grace-period=0`) - объект pod пропал, процесс на node
ещё может писать до реального завершения; краткоживущего стороннего писателя
между последним списком и apply; pod в другом namespace при общем PV
(RWX/hostPath-том). Гарантия ограничена так: на момент каждой проверки ни один
не завершённый pod namespace `brood` не ссылается на claim, кроме доказанных по
UID. Предпосылки, которые обеспечивает оператор: том RWO локального PV, node
недоступна посторонним, право создавать pod в `brood` и на `pods/exec`/node
ограничено, force-delete pod не используется во время выкладки. Закрывается
только admission-политикой кластера (ValidatingAdmissionPolicy на pod с этим
claim) и контролем доступа к node - решение оператора; обнаруженный чужой pod
всегда отказ. Сам snapshot (SQLite backup) остаётся согласованным и при
единственном живом писателе: список pod - дополнительная защита, не его замена.
Verify - отдельный deny-all Pod с read-only PVC: он сверяет
sha256 snapshot с метаданными snapshot и прогоняет тот же путь схемы, что
`store.Open` (версия, цепочка миграций и определения всех таблиц), на копии
байтов в памяти: шаги миграции пишут только в неё, на диск ничего не
создаётся и не пишется. Файл читается один раз через один дескриптор
(без симлинков, только обычный файл: открытие с O_NONBLOCK, FIFO отвергается, не больше 32 MiB при лимите helper 256Mi), его байты хешируются и затем открываются
SQLite как in-memory база: путь SQLite не передаётся, поэтому подмена файла
после чтения и посторонние `-wal`/`-journal`/`-shm` не влияют на вердикт,
а несовпадение sha256 - отказ. Это доказывает схему именно хешированных
байтов; sha256 из метаданных snapshot связывает verify со snapshot, а отсутствие
писателя даёт протокол выше. Immutable-флаг и `data_version` новой сессии SQLite
доказательством не считаются. Helper ждёт
оба терминальных статуса Pod (Succeeded и Failed) в пределах 190 с; отказ
называет операцию, причину из закрытого списка, код выхода и строку лога
только при совпадении с фиксированной грамматикой (схема, sha256), остальное
не печатается. Он выполняется в **новом** образе, значит
соответствие схемы проверяется именно кодом, который будет раскатан. Отказ
snapshot, verify, смена UID (Pod, ReplicaSet, Deployment, Service, PVC, PV)
или появление второго pod останавливают выкладку до apply. Образ с
несовместимой схемой или без миграции от версии базы не пройдёт verify.
Работает только если образ содержит
`database verify`; поэтому восстанавливающий PR должен быть revert поверх
main с этой командой, а не откатом ветки до старого дерева.

Чего это **не** доказывает. Упавший `Open` нового релиза мог выполнить SQLite
операции до отказа (прагма `journal_mode=WAL`, создание или checkpoint
`-wal`/`-shm`): утверждения «БД не изменена» нет. Snapshot фиксирует
фактическое состояние перед rollout; именно его sha256 и схему проверяет
verify. Ручной `rollout undo`, сброс БД и автоматический restore не
выполняются. После rollout действуют обычные inspect (счётчики users/admins
должны совпасть со snapshot) и operator-authenticated acceptance; вывод
добавляет строку о восстановлении.

Ни ошибка inspect, ни провал TLS probe не восстанавливают production DB
автоматически: новые внешние операции уже могли произойти. Возврат старого
образа на мигрированную базу не стартует; откат требует остановки записи и
внешних операций, явного восстановления snapshot и отдельного решения
оператора (раздел «Откат после миграции»). Хранение снимков
(`BROOD_DEPLOY_KEEP_SNAPSHOTS`) только удаляет старые снимки после успешной
выкладки и ничего не восстанавливает.

Путь forced SSH не меняется: установленный `brood-deploy` вызывает
`bin/brood deploy server --check` и `bin/brood deploy server` из нового дерева.
Ни новый SSH action/key, ни runner, ни обход `deploy go` не нужны. Так же
устроены preflight места и уборка после выкладки (#247). Единственная
интеграция в Go deployment — callback `before_apply(image)`, после build и всех
preflight, перед apply. Поддержка `/brood database` должна находиться в новом
образе; если её нет, existing mode откажет без server apply.

## Настройки

Секреты задаются только приватным `.env` релиза, который передаёт CI.

| Настройка | Назначение |
|---|---|
| `BROOD_SESSION_SECRET` | случайный ключ, минимум 32 символа |
| `BROOD_PUBLIC_HOST` | DNS-имя публичного входа, присутствующее в TLS certificate |
| `BROOD_HTTPS_PORT`, `BROOD_HTTP_PORT` | опубликованные host ports, задаются при создании kind |
| `BROOD_DEPLOY_MODE` | `initial` по умолчанию; явный `existing` сохраняет текущую Go-БД без CI login |
| `BROOD_ADMIN_USERNAME`, `BROOD_ADMIN_PASSWORD` | только initial admin и initial проверка доступа; пароль непустой, одной строкой, длину выбирает оператор |
| `WORKER_POOL_IMAGES` | JSON approved digest images: обязательный gvisor, необязательные microvm и подготовленный lxc |
| `CLIPROXY_URL`, `CLIPROXY_MANAGEMENT_KEY` | опциональный существующий backend `http://cliproxy:8317` и его server-only management key |
| `CLIPROXY_API_KEY` | необязательный клиентский ключ CLIProxyAPI для managed-акторов; только `.env`/защищённая CI-конфигурация и Secret сервера, не guest env |
| `BROOD_GIT_REMOTES` | необязательные remotes репозиториев проектов для Git gateway: egress сервера и legacy-токены; только Secret сервера, см. «Git remotes» |
| `BROOD_ACTOR_GIT_DOMAIN` | необязательный домен Git-идентичности акторов (#249): коммиты актора подписаны `brood <актор> <актор@домен>`; DNS-имя в нижнем регистре, без дефолта; без него актор идентичности не получает. Смена домена требует reset акторов, см. «Подтверждение workspace» в `docs/PROXMOX.md` |
| `BROOD_DEPLOY_MIN_FREE_GB` | необязательный предел свободного места раздела Docker, GiB, целое от 1; меньше - отказ до сборки (#247) |
| `BROOD_DEPLOY_BUILD_CACHE_GB` | необязательный предел кэша сборки Docker, GiB, целое от 0; после успешной выкладки - уборка старых образов и `docker builder prune` (#247) |
| `BROOD_DEPLOY_KEEP_SNAPSHOTS` | необязательное число снимков базы на PVC в `existing`, целое от 1, включая снимок этой выкладки (#247) |
| `BROOD_ACTOR_MODEL`, `BROOD_ACTOR_SMALL_MODEL` | необязательные модели Claude Code акторов (#263): основная и быстрых задач, например `glm-5.3` и `glm-5.3-flash`; каждая сама по себе, без дефолта, до 256 печатаемых ASCII без пробелов. Пустая — модель этой группы выбирает Claude Code. Гость передаёт их Claude Code (`ANTHROPIC_MODEL` и псевдонимы `opus`/`sonnet`, `haiku`), см. «Модели Claude Code актора» в `docs/PROXMOX.md`; смена доходит до актора со следующим стартом CT |
| `BROOD_TELEGRAM_BOT_TOKEN` | необязательный токен бота установки (#281) в форме BotFather `<id>:<секрет>`: бот пользователей без своего бота, свой приоритетнее. Секрет установки: только `.env` и защищённая переменная CI, не в логи, ответы API и guest env. Неверная форма — отказ запуска с именем настройки, без значения |
| `BROOD_TELEGRAM_API` | необязательный origin Bot API (#281), по умолчанию `https://api.telegram.org`; HTTPS или HTTP на loopback (локальный Bot API), без path, query и credentials. Им пользуются бот установки и свои боты пользователей |
| `BROOD_PANEL_ORIGIN` | необязательный origin панели, каким его открывает пользователь (#282): http(s) без credentials, path, query и fragment, например `https://brood.example:8443`. Уведомления Telegram ссылаются по нему на страницу роя; без настройки уведомления идут без ссылки. Неверная форма — отказ запуска с именем настройки |

Выкладка передаёт `BROOD_TELEGRAM_BOT_TOKEN`, `BROOD_TELEGRAM_API` и
`BROOD_PANEL_ORIGIN` в Secret и env сервера (#286). Egress к Bot API —
разрешение имени при каждой выкладке: kindnet умеет только IP-правила, и
смена адреса Telegram между выкладками останавливает уведомления до
следующей выкладки; локальный Bot API на loopback правил не требует.
Подробности и хрупкость — [TELEGRAM.md](TELEGRAM.md), «Egress к Bot API и
выкладка».

401 или 409 от Telegram останавливают бота установки для всех пользователей
без своего бота до перезапуска сервера: сам сервер его снова не запускает, а
причину показывает `bot_reason` профиля Telegram. Оператор устраняет
причину — 401: выпускает новый токен в BotFather и записывает его в
`BROOD_TELEGRAM_BOT_TOKEN`; 409: останавливает второй процесс, который
опрашивает того же бота `getUpdates`, или снимает его webhook
(`deleteWebhook`) — и перезапускает brood.

Переход акторов с псевдонимов Claude в CLIProxy на модели установки (#263):
задать `BROOD_ACTOR_MODEL` и `BROOD_ACTOR_SMALL_MODEL` в `.env` релиза,
выложить сервер, сделать reset акторов (новый CT получает гостя с поддержкой
моделей, см. «Артефакты бутстрапа песочницы») и проверить по журналу
CLIProxy, что запросы акторов идут с именами установки. После выкладки и
сброса акторов убрать псевдонимы моделей Claude из CLIProxy (#263):
оставленный псевдоним молча отвечает другой моделью на запрос имени Claude,
и журнал актора назовёт модель Claude.

Для отдельного backend managed-акторов задаются `BROOD_ACTOR_LLM_URL`
(строгий HTTPS origin) и `BROOD_ACTOR_LLM_API_KEY`. Необязательная
`BROOD_ACTOR_LLM_CA_PEM_B64` содержит base64 PEM доверенного CA; без неё
используются системные корни. Неполная конфигурация, некорректный CA или
небезопасный результат DNS отвергаются до сборки и записи.

Эти настройки передаются только через server Secret и `secretKeyRef`.
NetworkPolicy разрешает только найденные IP и точный TCP-порт backend;
смена DNS требует повторной выкладки. Ротация ключа меняет checksum pod
и вызывает rollout. Отсутствующие настройки удаляются из управляемых
ключей Secret и env. Установленные `CLIPROXY_URL`, management key, аккаунты
и список API-ключей proxy панели не заменяются. Гость получает только
локальный gateway, не upstream key или CA.

TLS: `secrets/tls.crt` и `secrets/tls.key`. Go и edge используют один
сертификат; upstream SNI `brood` требует соответствующего SAN. Проверка
public endpoint использует точный certificate и loopback resolve, не
`curl -k` и не случайный DNS destination.

В режиме `initial` после смены admin-пароля через UI прежний deployment check
credential больше не работает и следующая auth-проверка откажет. Для уже
работающей установки оператор может явно выбрать `existing`, не копируя свой
пароль в CI. Bootstrap в initial не меняет пароль существующей учётной записи.
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

## Место на машине деплоя (#247)

Выкладка собирает образы сервера и edge через `docker buildx` на самой машине
деплоя, и каждая сборка добавляет в кэш слои панели и Go. 2026-11-10 кэш вырос
до 60 ГБ (2069 записей, 0 активных), неиспользуемые образы - до ~10 ГБ, и
корневой раздел с `/var/lib/docker`, узлом kind и томом базы заполнился на 100%:
выкладка упала на snapshot, а запись SQLite работающего сервера оказалась под
угрозой. Три настройки `.env` релиза ограничивают рост. Пределы - свойства
машины (размер диска, тома, образов), поэтому дефолтов в коде нет: без
настройки шаг не выполняется, как до #247, а `bin/brood deploy server`
печатает `warning: ... is not set`.

| Настройка | Шаг | Без неё |
|---|---|---|
| `BROOD_DEPLOY_MIN_FREE_GB` | preflight: отказ до сборки, если на разделе каталога данных Docker свободно меньше N GiB | проверка пропущена, предупреждение |
| `BROOD_DEPLOY_BUILD_CACHE_GB` | после успешной выкладки: старые теги своих образов, затем `docker builder prune -f --keep-storage NGB` | уборки нет, предупреждение |
| `BROOD_DEPLOY_KEEP_SNAPSHOTS` | после успешной выкладки `existing`: на PVC остаются N снимков, включая снимок этой выкладки | снимки копятся, предупреждение |

Значения - целые числа ASCII: `MIN_FREE` от 1, `BUILD_CACHE` от 0 (весь
неиспользуемый кэш), `KEEP_SNAPSHOTS` от 1. GB здесь - 2^30 байт, как у
`df -h` и `--keep-storage`. Иная форма (`10GB`, `1.5`, пробел) - отказ уже
`deploy server --check`, до доступа к машине и кластеру, с именем настройки.

Значения для production (`BROOD_DEPLOY_ENV`, диск 99 ГБ):

```
BROOD_DEPLOY_MIN_FREE_GB=10
BROOD_DEPLOY_BUILD_CACHE_GB=5
BROOD_DEPLOY_KEEP_SNAPSHOTS=5
```

10 GiB оставляют место для одной сборки и роста базы; 5 GiB кэша - значение,
которым 2026-11-10 освободили 56 ГБ: `--keep-storage` удаляет давно не
использованные записи первыми, так что базовые образы и слои `go mod
download`/`bun install`, которые сборка берёт каждый раз, уходят последними.
Снимок прошедшей выкладки не больше 32 MiB (предел verify), пять таких - не
больше 160 MiB. Снимки отказавших запусков (schema gate, `snapshot is too large
to verify in memory`, любой отказ до apply) этим пределом не ограничены и
копятся по одному на попытку до следующей успешной выкладки.

Порядок и что удаляется:

1. **Preflight.** `deploy server --check`, а затем `deploy server` первым шагом,
   до Lease, сборки и любой записи (на полном диске не пишется и Lease),
   читают `docker info --format {{.DockerRootDir}}` и `df -P -k` этого
   каталога. Свободно меньше предела - отказ с точкой монтирования, свободным
   местом и пределом; ради причины дочитывается `docker system df` (кэш сборки
   и образы с reclaimable; при успехе он не вызывается: обходит тома и
   медленный). Каталог не найден или `df` не прочитан при заданной настройке -
   тоже отказ. Preflight ничего не удаляет:

   ```
   free space on / (Docker root) is 3.0 GiB, below BROOD_DEPLOY_MIN_FREE_GB=10 GiB;
   build cache 60.2GB (reclaimable 60.2GB); images 16.7GB (reclaimable 10.1GB (60%));
   nothing was built or changed; free space first, for example docker builder
   prune -f --keep-storage 5GB as the deploy user, and set
   BROOD_DEPLOY_BUILD_CACHE_GB so that every deploy prunes the build cache
   (docs/DEPLOY.md)
   ```

2. **Снимки базы** (только `existing`, после public probe, ещё под Lease
   `brood-go-deploy`). Helper создаётся тем же путём `existing_install.py`, что
   snapshot, verify и inspect: точный digest нового образа, deny-all
   NetworkPolicy, без service-account token, non-root, read-only rootfs, node
   текущего pod, только owned PVC (на запись, как у snapshot), удаление pod и
   policy по UID. Он выполняет `/brood database prune --current
   /data/.brood-snapshot-<hex>.db --keep N`: остаются снимок этой выкладки и
   N-1 самых новых прочих по mtime, удаляются (`unlink`) остальные обычные
   файлы `/data/.brood-snapshot-<32 hex>.db`. Не трогаются `brood.db` и его
   `-wal`/`-shm`, `artifacts/`, служебные файлы SQLite, симлинки (по ним не
   ходят), каталоги и файлы других имён. Снимок этой выкладки обязан быть
   обычным файлом до удалений - иначе отказ до первого из них - и после них -
   иначе отказ helper; ответ helper (`current`, `kept`, `removed`, без имён
   прочих файлов) обязан назвать именно его, `kept` не больше N. Снимки отказавших запусков - те же
   `.brood-snapshot-*.db` и уходят в общем порядке. В `initial` снимка нет, шаг
   не выполняется.
3. **Docker** (оба режима, после снятия Lease, только после успеха): `docker
   image ls` и `docker image rm repo:tag` без `--force` для тегов
   `localhost:5002/brood-go:build-<32 hex>` и
   `localhost:5002/brood-go-edge:build-<32 hex>`, кроме тега этой выкладки и
   одного тега самого нового иного образа того же репозитория (предыдущего).
   Тег того же образа, что текущий (edge без изменений собирается из кэша),
   только снимается. Затем `docker builder prune -f --keep-storage NGB`: его
   принимают классический `docker builder` (Debian 13) и buildx; в buildx 0.17+
   это устаревшее имя `--reserved-space` (печатает предупреждение), которое без
   других пределов тоже сокращает кэш до N. Удалят флаг - prune откажет с
   причиной в предупреждении. Не трогаются образы
   kind, `registry`, `ateworker-*`, теги не вида `build-<hex>`, чужие
   репозитории, контейнеры и тома (узел kind и PVC), содержимое реестра
   `localhost:5002`; `docker image prune` и `docker system prune` не
   вызываются. Тег этой выкладки неизвестен или его нет в списке, список не
   читается - ни одного удаления образов, но кэш сборки всё равно чистится.

Отказ самого prune (helper Failed, например `cannot remove an old snapshot`, или
ответ не про снимок этой выкладки) и любой сбой шага 3 (код docker, бинарь не
запустился) не отменяют выкладку: rollout и probe уже прошли. Они печатаются
как `warning: snapshot retention failed: ...` или `warning: Docker cleanup
failed: ...` с причиной, Lease снимается как обычно, команда завершается успехом
и `releases/current` переключается; следующую выкладку при нехватке места
остановит preflight. Сбой жизненного цикла helper шага 2 (identity Deployment
или helper изменилась, pod или policy не удалены по UID, ошибка API или ОС) -
другое: helper с записываемым томом мог остаться, поэтому это отказ после
`before_apply`, как у inspect: Lease остаётся, действуйте по «Ручному снятию
Lease». Успех выглядит так:

```
Free space: 52.3 GiB on / (Docker root /var/lib/docker), required 10.0 GiB
Snapshot retention: kept 5 of BROOD_DEPLOY_KEEP_SNAPSHOTS=5 including this deploy's snapshot, removed 12
Docker cleanup: removed 37 old release image tags, kept ...; build cache pruned to 5 GiB, reclaimed 2.1GB
```

Почему в `bin/brood deploy server`, а не новой строкой `deploy/brood-deploy.sh`.
Ключ выкладки по-прежнему умеет одно: forced command `brood-deploy`, который
вызывает `bin/brood deploy server --check` и `bin/brood deploy server` из
нового релиза. Уборка - часть этого действия, а не новое SSH-действие, ключ
или шелл, как snapshot и schema gate #220. Копию `/usr/local/bin/brood-deploy`
обновляет только provision от root, а код релиза действует со следующей
выкладкой без входа на машину. Настройки читает общий парсер `.env` (кавычки
mop), а не его повтор на shell.

Если диск уже полон: от пользователя brood на машине деплоя
`docker builder prune -f --keep-storage 5GB` (так освободили 56 ГБ
2026-11-10), затем обычная выкладка с настройками выше.

Чего это не покрывает: содержимое реестра `localhost:5002` (каждая выкладка
пушит новый образ; сборка мусора реестра - отдельная операция), Go build cache
пользователя brood после сборки артефактов бутстрапа (Go сам удаляет записи,
не использованные 5 дней), `releases/` на разделе, отличном от раздела Docker,
образы воркеров и guest images, тома. Preflight меряет раздел `DockerRootDir`:
при хранилище образов containerd (`/var/lib/containerd`) на отдельном разделе
тот раздел не проверяется. Команды docker уборки идут без своего таймаута:
зависание после снятия Lease закончится таймаутом CI без переключения
`releases/current`.

## Git remotes

`BROOD_GIT_REMOTES` — секрет установки в `.env` релиза: JSON-объект, ключ —
канонический HTTPS URL репозитория, значение — `{}` (без учётных данных) или
ровно `{"username": "...", "token": "..."}` видимым ASCII без пробелов.
Канонический URL: `https://`, DNS-имя в нижнем регистре (не IP, без точки в
конце), без явного порта (только 443), userinfo, query, fragment и
percent-encoding; непустой путь из сегментов `[A-Za-z0-9._~-]` без `.`, `..`,
пустых сегментов и завершающего `/`; не длиннее 1024 байт. Повтор ключа —
отказ, а не молчаливое вытеснение.

Пустая настройка или `{}` оставляют выкладку прежней: ключа нет ни в Secret,
ни в env, DNS не разрешается, egress не добавляется. Иначе форму проверяет
уже `deploy server --check` (без DNS), а Go-выкладка до сборки образа и записи
в кластер разрешает каждый различный host remotes тем же resolver, что Proxmox
endpoints. Значение целиком уходит в `data` Secret `brood-go-env` и серверу
через `secretKeyRef`; NetworkPolicy сервера получает `/32` или `/128` каждого
найденного адреса только на TCP 443. Частные адреса LAN разрешены: host задал
оператор. Loopback, link-local, multicast, unspecified, reserved адрес, пустой
ответ или ошибка DNS — отказ. Отказ называет настройку, но не URL, host или
token. Ротация токена меняет checksum `Secret.data` и перезапускает сервер;
режим `existing` переносит ключ тем же rollout после snapshot. Смена адресов
host требует повторной выкладки. Токен не попадает в argv, вывод, edge,
акторов и воркеров.

Токен в `BROOD_GIT_REMOTES` больше не обязателен: проектный Git credential
(`bro secret git`, [SECRETS.md](SECRETS.md)) хранится в приватной БД сервера,
ротируется без выкладки и имеет приоритет над токеном remote того же
repository. Выкладка не читает БД и не знает его host, поэтому egress сервера
к нему по-прежнему открывает только ключ `BROOD_GIT_REMOTES`: для repository
с проектным credential достаточно значения `{}`. Без egress шлюз отвечает
502, хотя режим origin — `gateway`.

## Proxmox runtime

`BROOD_PROXMOX_POOLS` и `BROOD_PROXMOX_TOKENS` передаются парой как JSON maps
в `data` серверного Secret `brood-go-env`. Карты содержат endpoint/CA и
credentialRef/token соответственно; полный контракт проверяет Go-сервер.
Без runtime-полей существующая config-only установка не требует новых
настроек, не разрешает DNS endpoints при выкладке и не получает API egress.

Наличие любого из `template`, `templateDigest`, `capacity` на верхнем уровне
пула включает runtime-проверку выкладки. Для этого нужны две явные настройки:

| Настройка | Назначение |
|---|---|
| `BROOD_PUBLIC_ORIGIN` | строгий HTTPS origin без credentials, path (включая `/`), query и fragment |
| `BROOD_PROXMOX_RELAY_SECRET` | отдельный случайный relay key, минимум 32 UTF-8 байта, не равный `BROOD_SESSION_SECRET` |

При явном указании этой пары она проверяется и передаётся серверу даже без
runtime-пула. Одиночная настройка отвергается. Значения не выводятся и не
передаются build args, edge, акторам или воркерам: только server Secret и
точечные `secretKeyRef` серверного контейнера. Детерминированный SHA-256 всего
`Secret.data` в pod template перезапускает сервер также при ротации relay key
или origin с прежним image digest. Удаление настроек сохраняет прежнее
владение `data`-ключами через apply; чужие ключи не затрагиваются.

До сборки и записи deploy разрешает только endpoints runtime-пулов из
`BROOD_PROXMOX_POOLS` через `socket.getaddrinfo` (инъецируемый resolver в тестах).
Endpoint должен быть HTTPS origin; порт берётся из URL, без явного порта — 443,
не неявный Proxmox 8006. Ошибка DNS, пустой ответ, loopback, unspecified,
multicast, link-local или reserved адрес останавливают выкладку без значений
endpoint, CA или токенов в диагностике. Частные unicast IP разрешены.

Серверная NetworkPolicy сохраняет native DNS/Substrate/Kubernetes API,
registry и proxy правила и добавляет отдельные TCP egress правила: каждый
найденный IPv4 `/32` или IPv6 `/128` только на точный порт своего endpoint.
Широких CIDR, host networking и новых actor/worker разрешений нет. Изменение
DNS требует новой выкладки: список адресов фиксируется в NetworkPolicy.
Публичный TLS остаётся существующим `secrets/tls.crt`/`secrets/tls.key`;
дополнительная настройка guest CA этим слоем не вводится.

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

## Артефакты бутстрапа песочницы

До snapshot и apply выкладка собирает `brood-guest` и `bro` из релизного
дерева, скачивает `claude` по версии из `brood/dev/pins.json` только по HTTPS
и сверяет SHA-256 с пином. До 20 с на соединение и до 150 с на попытку:
только тайм-аут разрешает продолжить тот же приватный частичный файл, всего
не более трёх попыток. Ошибки загрузки и несовпадение SHA-256 различаются;
тело ответа не выводится. Отказ подготовки
не запускает snapshot или apply сервера и в `existing` освобождает Lease
по обычному пути отказа до `before_apply`. После подготовки выкладка повторно
проверяет принадлежность namespace и всех целей apply, затем запускает snapshot.

Только после успешного rollout выкладка публикует все три артефакта с
`manifest.json` в PVC сервера (`/data/artifacts`, настройка
`BROOD_ARTIFACTS_DIR`). Образ сервера — scratch
без shell, поэтому поток разбирает сам бинарь: `kubectl exec -i
pod/<имя> -- /brood artifacts --dir /data/artifacts`. Сервер
перечитывает манифест на каждый запрос гостя: новый CT забирает новые бинари
без рестарта сервера. Работающий актор продолжает прежний набор до своего
следующего старта CT. В CT из шаблона с bootstrap #184 этот старт ставит
новый набор рядом с прежним и переключается на него с сохранением home и
workspace; выкладка прежнего релиза возвращает прежний набор тем же
stop/start (`docs/PROXMOX.md`, «Обновление гостевых бинарей»). CT прежних
шаблонов после выкладки, меняющей артефакты, stop/start не переживают: их
переводят на новый шаблон по разделу «Новый шаблон гостя и переход пула».

Под публикации выбирается явно, после rollout и прямо перед exec (#246):
`kubectl exec deployment/brood-go` выбирает под сам и после rollout попал в
старый, уже завершающийся; поток висел 48 минут. Цель — ReplicaSet текущей
ревизии Deployment (`deployment.kubernetes.io/revision` при `observedGeneration`
не ниже `generation`) с controller-ссылкой на UID Deployment, и его под:
controller-ссылка на UID этого ReplicaSet, его `pod-template-hash`, фаза
`Running`, условие `Ready`, без `deletionTimestamp`. Одна метка
`pod-template-hash` не доказательство, её может нести любой под; UID владельцев
ставят контроллеры. Такой под ровно один, иначе отказ до exec с ревизией, числом
и именами кандидатов. Exec ограничен общим таймаутом 600 с на весь поток около
310 МБ (`ARTIFACT_PUBLISH_TIMEOUT`, `brood/dev/deploy/go_server.py`; в норме
поток уходит за десятки секунд). По таймауту `kubectl` убит, отказ —
`guest bootstrap artifact publish timed out after 600 s`. `brood artifacts`
заменяет каталог rename только после полного потока: на томе прежний набор
или новый целиком, частичного нет.

Отказ публикации (таймаут, нет единственного готового пода, ошибка exec)
приходит после apply: сервер уже выкачен, но новые акторы получат прежний
гостевой runtime, пока публикация не пройдёт. Что делать:

1. Убедитесь, что процесс deploy завершился и `kubectl exec` не висит
   (`pgrep -af 'brood deploy server'`, `pgrep -af 'kubectl.*exec'` на машине
   деплоя пусты; таймаут убивает exec сам).
2. Отказ про под — сначала `kubectl -n brood get pods -l app=brood-go` и
   `kubectl -n brood rollout status deployment/brood-go`: незавершённый rollout
   или лишний под разбирают до повтора, а не повторяют вслепую.
3. В режиме `existing` Lease `brood-go-deploy` после такого отказа остаётся
   (исход после `before_apply`, раздел «Существующая Go-установка»): снимите её
   по процедуре «Ручное снятие Lease» с preconditions uid/resourceVersion.
   В `initial` Lease нет.
4. Повторите выкладку тем же релизом (перезапуск job): она проходит заново
   целиком, от snapshot и schema gate до публикации артефактов.
