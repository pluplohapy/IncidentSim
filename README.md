# IncidentSim

IncidentSim — тренажёр разбора инцидентов на Go. Участник
получает работающий, но неисправный сервис, наблюдает симптомы через логи,
метрики, shell и SQL, затем исправляет причину в workspace.

Проверка состоит из двух независимых шлюзов: correctness отвечает на вопрос
«работает ли сервис сейчас», quality — «годится ли исправление для продакшена».
Исправление, которое лишь маскирует симптом, получает `partial_fix`.

Docker Compose — основной движок разработки и CI. На Linux-хосте с KVM
доступен Firecracker; на текущем macOS/KVM-less окружении он не проверяется.
Teleport остаётся опциональным выключенным адаптером и не входит в основной путь.

## Быстрый старт

```console
make build
bin/incidentsim serve
bin/incidentsim list
bin/incidentsim start db-connection-leak
bin/incidentsim status <session>
bin/incidentsim edit <session> app/handler.go
bin/incidentsim apply <session>
bin/incidentsim verify <session>
bin/incidentsim postmortem <session> --md
bin/incidentsim stop <session>
```

`start` печатает путь workspace под `<state>/sessions/<session>/workspace`.
`apply` берёт именно его текущее содержимое; `--file` и `--content` оставлены
только для CI/selfcheck API. Разрешены также формы `--session <session>`.

Основные команды участника: `show`, `start`, `status`, `logs`, `metrics`,
`env`, `exec`, `sql`, `shell`, `edit`, `diff`, `apply`, `verify`, `hint`,
`reset`, `reveal`, `postmortem`, `stop`.

## Задача

Манифест в `manifests/<id>.yaml` содержит публичные симптомы, editable-файлы,
профиль replay и проверки. Закрытые поля (`root_cause`, `theory`, reference и
distractors) доступны только после `pass`, явного `give-up` или через
административный API с `INCIDENTSIM_ADMIN=1`.

Workspace и эталон находятся в `scenarios/<id>/`. Для code-задачи Dockerfile,
`go.mod` и `go.sum` не редактируются; reference отличается от workspace только
в editable-файлах. Полный контракт — в [docs/adding-a-task.md](docs/adding-a-task.md).

Текущий проверенный каталог задач:

| ID | Уровень | Стенд |
|---|---|---|
| `cache-avalanche-retry-storm` | composite | app + cache + db |
| `db-connection-leak` | code | app + PostgreSQL |
| `log-flood-disk` | config | app + tmpfs |
| `pg-table-bloat` | db | app + PostgreSQL |

Пул из 24 code-level стендов из проектного плана ещё не закрыт: эти четыре —
единственные задачи, для которых в checkout есть реальные стенды и манифесты.
Новую задачу можно начать через `scripts/new-task.sh`, но TODO нужно заменить
рабочим стендом до включения в каталог.

## Разработка

```console
go build ./...
go vet ./...
go test -race ./...
bin/incidentsim validate manifests
```

На macOS и Linux нужен доступный Docker daemon с Compose v2. Порты сервисов
публикуются только на `127.0.0.1`; порт PostgreSQL наружу не публикуется.

Для Firecracker на Linux установите pinned release `1.11.0` через
`ansible/firecracker.yml`, затем запустите ядро с `engine: firecracker` и
проверьте `incidentsim selfcheck <task>`. На реальном KVM-хосте этот checkout
не проверен; локальный контрактный тест включается через
`INCIDENTSIM_TEST_FIRECRACKER=1`.
