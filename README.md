# brood

Пул агентов на [Agent Substrate](https://github.com/agent-substrate/substrate):
оркестратор раздаёт работу акторам. Наследник mop: трекер, CI и master-loop,
портированные на Go.

## Установка

Требуется Go 1.27+.

```sh
cp ~/mop/.env.example .env 2>/dev/null || true   # или создайте свой
# GITLAB_SITE='git.ermak.dev'
# GITLAB_TOKEN='glpat-...'   # личный токен с правом api

go build -o bin/brood ./cmd/brood
```

Координаты трекера берутся из git origin рабочей копии; `.env` переопределяет.

## Команды

```sh
brood dev bug list          # открытые задачи, тяжёлые первыми
brood dev bug show <iid>    # тело задачи
brood dev bug new "<title>" --label type::bug --body-file body.md
brood dev bug start <iid>   # взять: status::wip + ветка
brood dev bug close <iid> --comment "..."
brood dev ci                # последние пайплайны
brood dev ci why <id>       # почему красный
brood dev ci log <job>      # лог джобы
brood dev ci lint           # проверка .gitlab-ci.yml
```

Полная справка — `brood` без аргументов; правила разработки — `CLAUDE.md`.
