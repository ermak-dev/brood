# brood/app — прикладная авторизация (#31)

Прикладной слой проектов, членства и роёв поверх хранилища и провайдера
atespace'ов. Реализует правила доступа эпика #27 и операции #31.
Пакет молчит: данные и ошибки возвращает, печатает только внешний слой
(HTTP API, CLI).

## Состав

- `store.go` — типы `User`, `Project`, `Membership`, `Swarm`,
  `SwarmState` и интерфейс `Store`, который реализует хранилище #29
  (таблицы users, projects, project_members, swarms) напрямую или через
  адаптер. Интерфейс минимален: только то, что нужно прикладному слою.
- `service.go` — `Service` с зависимостями: `Store`, `Provisioner`
  (граница Substrate, реальный адаптер — #32), `Clock` (UTC) и `IDGen`
  (строковые ULID/UUID).
- `authz.go` — `Can(user, action, resource)`: действия `view-project`,
  `create-swarm`, `view-swarm`, `manage-swarm`, `delete-swarm`,
  `transfer-ownership`, `manage-users`, `manage-projects`,
  `manage-membership`; ресурсы — проект или рой по ID.
- `operations.go` — `CreateProject`, `AddMember`, `RemoveMember`,
  `SetUserDisabled`, `CreateSwarm`, `DeleteSwarm`, `TransferSwarm`,
  `GetSwarm`, `ListProjectsFor`, `ListSwarmsFor`, `AdminProjects`,
  `AdminSwarms`.
- `errors.go` — ошибки-значения (`ErrNotAllowed`, `ErrNotFound`,
  `ErrExists`, `ErrConflict`, `ErrState`), отображаются статусами HTTP
  или текстом CLI, без трассировок.

## Правила доступа

- Заблокированный пользователь всегда отказан, включая администратора.
- Активный администратор управляет пользователями, проектами,
  членством и всеми роями, кроме deleted.
- Просмотр проекта и создание роя требуют активного членства в проекте;
  у администратора без членства их нет, полный список — `AdminProjects`.
- Рой доступен владельцу, пока он участник проекта роя, и админу.
  Членство само по себе чужие рои не открывает.
- Исключение из проекта отзывает доступ, но не удаляет рои.
- `manage-swarm` покрывает акторов, терминал и политики; операции
  акторов проверяют его же (#32/#33). Управление и передача владения —
  только роям в `active`; просмотр — всем состояниям, кроме `deleted`;
  удалённый рой не существует ни для кого.
- Защита последнего активного администратора — слой #30, здесь не
  проверяется.

## State machine роя

```
provisioning -> active | error
active       -> deleting | error
deleting     -> deleted | error
error        -> active | deleting | error | deleted
deleted      — терминальный, строка остаётся со state=deleted
```

Сбой Substrate — не отказ вызова, а состояние: строка роя получает
`state=error` и поле `Retry` (`create`/`delete`); повтор того же вызова
доводит операцию до конца. `CreateSwarm` идемпотентен по имени в
проекте: незавершённое создание того же владельца продолжается, чужое
имя — отказ. `DeleteSwarm` идемпотентен и сверяет имя atespace
по сохранённому UID; чужая одноимённая область не удаляется и не
присваивается (путь reconcile: `GetAtespace` перед `CreateAtespace`).

`atespace_name` строится только из ID роя:
`swarm-<id без дефисов, lower>` (`AtespaceName`), пользователь не может
подменить область через payload.

## Тесты

`fakes_test.go` — внутрипроцессные `memStore` и `fakeProvisioner`;
`app_test.go` — табличные тесты всех правил, включая межпользовательские
отрицательные сценарии, заблокированных пользователей и восстановление
после сбоев. Запуск: `go test ./brood/app/...` из `go/`.
