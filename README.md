# IncidentSim

Тренажёр разбора инцидентов. Участник получает задачу-инцидент в изолированной
среде, смотрит логи и метрики, ищет корневую причину и чинит. Проверка идёт на
двух уровнях: **correctness gate** (система работает сейчас) и **quality gate**
(решение уместно в проде и не маскирует симптом).

> Главный принцип: **«работает ≠ правильно»**.
> Инженер, который выключил логи чтобы «починить» log-flood, проходит correctness
> gate и проваливает quality gate.

Этот документ — спецификация. Диаграммы, инструкции запуска на bare-metal и
демо-сценарий живут в [`docs/`](docs/) и дополняют, но не заменяют спецификацию.

---

## 1. Что строим

* Задачи описываются YAML-манифестами; ядро читает манифест и вызывает подсистемы.
* Каталог задач расширяется без правки ядра: новый манифест — новая задача.
* Изоляция: Firecracker microVM (KVM) с Docker-образами в качестве rootfs
  (read-only базовый слой + writable overlay). Учебный сервис не переписывается
  под VM.
* Fallback для разработки: Docker Compose, реализующий тот же интерфейс рантайма.
* Опционально: Teleport как адаптер аудита сессий, вне основного пути MVP.

---

## 2. Архитектура

```
                    ┌────────────┐
                    │    cli     │  incidentsim start|verify|...
                    └─────┬──────┘
                          │ HTTP (localhost)
                    ┌─────▼──────┐
                    │   core     │  оркестратор, event log, postmortem
                    │  ┌────────┐│
                    │  │catalog ││  manifests/*.yaml
                    │  └────────┘│
                    └──┬───┬───┬─┘
              injector│   │   │verifier
                    ┌─▼───▼───▼──┐
                    │  runtime   │  интерфейс: up/exec/logs/metrics/reset/apply
                    │  Adapter   │
                    └──┬──────┬──┘
            firecracker│      │compose
              ┌────────▼──┐ ┌─▼─────────────┐
              │microVM+KVM│ │docker compose │
              └───────────┘ └───────────────┘
```

Компоненты:

| Компонент      | Отвечает за                                                              |
|----------------|--------------------------------------------------------------------------|
| `core`         | HTTP API, оркестратор сессий, загрузчик манифестов, журнал событий, генератор постмортема |
| `runtime`      | Интерфейс `up / exec / logs / metrics / reset / apply` + реализации `firecracker` и `compose` |
| `injector`     | Активация сбоя по манифесту: `feature_flag`, `sql_config`, `kill`, `network`, `stress` |
| `verifier`     | Replay-нагрузка, сбор состояния, двухуровневые проверки, сверка с `root_cause` |
| `cli`          | Тонкий клиент к API ядра                                                  |
| `manifests/`   | YAML-задачи                                                              |
| `workloads/`   | replay-профили                                                           |
| `ansible/`     | playbooks провижининга Firecracker                                       |

Ansible отвечает за установку Firecracker, сеть (tap/bridge), systemd-юниты для
microVM, провижининг rootfs и снапшотов, деплой базового baseline-снапшота.
Снапшоты Firecracker дают быстрый reset задачи: restore вместо cold boot.

---

## 3. Структура репозитория

```
cmd/incidentsim            CLI
cmd/incidentsim-workload   simulated учебный сервис (образ для compose/microVM)
internal/core              API, оркестратор, журнал, постмортем
internal/manifest          схема манифеста, JSON Schema, загрузчик и валидация
internal/runtime           интерфейс рантайма + фейк для тестов
internal/injector          интерфейс и реализации инъекторов
internal/verifier          двухуровневые проверки, replay, policy-проверки
internal/adapters/compose  compose-реализация рантайма (fallback для разработки)
internal/adapters/firecracker  Firecracker-реализация рантайма (основная)
internal/adapters/teleport адаптер аудита (опционально, stub в MVP)
internal/cli               команды CLI
manifests/                 задачи
workloads/                 replay-профили
ansible/                   playbooks и роли Firecracker
skills/                    скиллы процесса (см. skills/README.md)
docs/                      диаграммы, demo, инструкции
```

---

## 4. Схема манифеста (фиксированная, не DSL)

Полное описание полей — [`skills/manifest-authoring/SKILL.md`](skills/manifest-authoring/SKILL.md)
и [`internal/manifest/schema.json`](internal/manifest/schema.json). Каркас:

```yaml
apiVersion: incidentsim/v1
kind: Incident
metadata:
  id: db-connection-leak
  title: "Пул соединений к БД исчерпан"
  level: code                     # config | code | db | composite
spec:
  workspace: scenarios/<id>/workspace   # копируется в сессию при start
  reference: scenarios/<id>/reference   # эталонное решение
  services: [...]                 # образы, env, порты, healthcheck
  editable: [...]                 # файлы, которые участник может править
  fault:                          # что активирует injector (опционально для code-level)
    type: sql_config
    target: db
    options: { ... }
  fault_chain: [...]              # только для composite
  distractors: [...]              # «чинящие» симптом, но не корень (каждый указывает fails: <check-id>)
  verify:
    correctness: [...]            # система работает сейчас
    quality: [...]                # решение уместно в проде
    replay:                       # нагрузка перед проверками
      profile: workloads/<id>.yaml
      duration: 75s
      rps: 20
      target_service: app         # явная цель replay (опционально)
  root_cause: { ... }
  reference_solution: { ... }
  theory: "..."
  # Вердикты: pass, fail, partial_fix (correctness=pass, quality=fail)
```

Задачи в каталоге:

| id | Уровень | Что сломано | Чем ловится маскировка |
|----|---------|-------------|------------------------|
| `log-flood-disk` | config | debug-логи без ротации | policy на `LOG_LEVEL=info`: удаление лога проходит correctness и падает на quality |
| `db-connection-leak` | code | незакрытая транзакция в пуле: соединения висят в `idle in transaction` | метрика `app_db_pool_in_use` под нагрузкой + SQL на `idle in transaction` |
| `pg-table-bloat` | db | мёртвые кортежи, autovacuum отключён | SQL: данные на месте и autovacuum включён |
| `cache-avalanche-retry-storm` | composite | кэш выключен, ретраи без backoff | policy: ретраи **ограничены**, а не выключены |

> Ограничение Firecracker-движка: `apply` с `--file` или сменой образа (code-level задачи) не поддерживается. Firecracker-применение принимает только переменные окружения. Code-level задачи требуют compose-движка, так как фикс пересобирает Docker-образ из рабочей директории участника (`~/.incidentsim/sessions/<id>/workspace/`).

Подробности задачи — [`docs/adding-a-task.md`](docs/adding-a-task.md).

---

## 5. Этапы работы

Каждый этап — отдельная ветка, наследующая предыдущую (stacked branches).
Внутри — атомарные коммиты: один коммит = одна подзадача, каждый коммит
оставляет код в проходящем состоянии. Ветка — не более 500 изменённых строк
кода (без сгенерированных файлов, lock-файлов и форматирования).

| Ветка                   | Содержание                                                              |
|-------------------------|-------------------------------------------------------------------------|
| `feat/skeleton`         | структура, `go.mod`, `Makefile`, CI, README со спецификацией            |
| `feat/manifest-schema`  | структуры манифеста, JSON Schema, парсинг и валидация YAML              |
| `feat/interfaces`       | интерфейсы runtime / injector / verifier + заглушки                    |
| `feat/compose-adapter`  | compose-реализация рантайма: up, exec, logs, restart                    |
| `feat/core-cli`         | HTTP API, сессия, event log, CLI start/metrics/logs/verify              |
| `feat/firecracker-adapter` | Firecracker REST API, vsock-агент, snapshot/restore, Ansible         |
| `feat/injector-basic`   | инъекторы `feature_flag` и `sql_config`                                 |
| `feat/verifier-replay`  | replay-нагрузка, SQL-проверки, policy-проверки                          |
| `feat/content-mvp`      | три задачи: log-flood-disk, db-connection-leak, pg-table-bloat          |
| `feat/content-fault-chain` | `cache-avalanche-retry-storm`, chain-aware проверки                 |
| `feat/postmortem`       | генератор черновика постмортема из event log                            |
| `feat/teleport-audit`   | адаптер аудита сессий (опционально)                                     |
| `feat/teleport-audit`   | адаптер аудита сессий (опционально)                                     |
| `feat/docs-demo`        | диаграммы, инструкция запуска, демо-скрипт                              |

---

## 6. Критерии готовности MVP

1. `incidentsim start <id>` поднимает microVM (или compose fallback), активирует
   сбой, сессия живёт.
2. `incidentsim verify` запускает replay и возвращает pass/fail с детализацией
   по correctness и quality.
3. Три задачи (`log-flood-disk`, `db-connection-leak`, `pg-table-bloat`) и одна
   многосоставная (`cache-avalanche-retry-storm`).
4. `incidentsim reset` восстанавливает baseline через snapshot (Firecracker) или
   down/up (compose).
5. `incidentsim postmortem` отдаёт черновик с таймлайном и пометкой о частичном
   фиксе.
6. README объясняет, как поднять на bare-metal с KVM и как добавить задачу.

---

## 7. Чего не делаем

* Не пишем «универсальный DSL» для манифестов — фиксированная схема.
* Не делаем UI на MVP.
* Не подключаем Kubernetes.
* Не пытаемся автоматически «понимать» код участника: проверяем поведение и
  артефакты, а не стиль.
* Не открываем PR и не делаем force-push в основную ветку.

---

## 8. Команды

```console
$ make build            # собрать bin/incidentsim и bin/incidentsim-workload
$ make test             # unit-тесты (с -race)
$ make check            # fmt + vet + test, то же что в CI
$ bin/incidentsim show  # список задач каталога
$ bin/incidentsim start db-connection-leak
$ bin/incidentsim metrics --session <id>
$ bin/incidentsim apply --session <id> --service app --file app/main.go --content '<код>'
$ bin/incidentsim apply --session <id> --service app --set POOL_MAX=100   # дистрактор: не проходит quality
$ bin/incidentsim verify --session <id>
$ bin/incidentsim reset --session <id>
$ bin/incidentsim postmortem --session <id> --md
$ bin/incidentsim selfcheck db-connection-leak   # reference: pass, distractors: partial_fix
$ ./scripts/demo.sh log-flood-disk     # интерактивное демо обеих проверок
```

Подробности запуска:

* на macOS (Apple Silicon) — [`docs/local-macos.md`](docs/local-macos.md): Colima +
  Docker Compose, одна команда `./scripts/demo.sh`;
* на bare-metal Linux с KVM — [`docs/runbook.md`](docs/runbook.md): провижининг
  Ansible и движок Firecracker.

Firecracker на macOS недоступен: `/dev/kvm` — часть ядра Linux, вложенной
виртуализации на Apple Silicon нет. Это ограничение платформы, а не настройка.
