# brood

Пул агентов на [Agent Substrate](https://github.com/agent-substrate/substrate):
оркестратор раздаёт работу акторам. Наследник mop: фичи переносятся из него
по частям, на Python, с тем же устройством командлетов. Сервер установки —
проект Django.

## Установка

Требуется Python 3.12+ с модулем venv.

```sh
cp ~/mop/.env.example .env 2>/dev/null || true   # или создайте свой
# GITLAB_SITE='git.ermak.dev'
# GITLAB_TOKEN='glpat-...'   # личный токен с правом api

ln -s "$PWD/bin/brood" ~/bin/brood
brood dev setup              # venv/ с Django и ruff; DJANGO_SECRET_KEY и DJANGO_DEBUG в .env
brood server migrate
brood server createsuperuser
brood server runserver       # админка: http://127.0.0.1:8000/admin/
```

`bin/brood` сам активирует `venv/` рабочей копии, в которой его зовут.
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
brood server <command>      # manage.py сервера: migrate, shell, test, ...
brood server test           # все проверки brood
brood substrate install     # локальный Substrate в kind из официального репозитория
brood substrate stop        # остановить, данные кластера остаются
brood substrate start       # поднять и дождаться готовности
brood substrate uninstall   # удалить вместе с данными
brood deploy server         # сервер с Postgres в кластер локального Substrate
brood dev ci set --file VAR=<файл>   # защищённая переменная CI
brood dev ci vars           # переменные CI без значений
```

Локальный Substrate ставится из github.com/agent-substrate/substrate на
закреплённом коммите; свой клон — `SUBSTRATE_SOURCE` в `.env`. Подробности —
`docs/SUBSTRATE.md`.

Полная справка — `brood` без аргументов и `brood <группа>`; правила
разработки — `CLAUDE.md`.
