# Добавление новой задачи в IncidentSim

## Структура задачи

Каждая задача живет в `scenarios/<id>/` и содержит:

```
scenarios/<id>/
├── workspace/              # разбитая рабочая копия (копируется в сессию при start)
│   └── ...                 # файлы, которые участник может править
├── reference/              # эталонное решение (используется selfcheck)
│   └── ...                 # reference-файлы (app/main.go, config/app.conf, solution.sql)
└── (для code-задачи) Dockerfile, go.mod, go.sum
```

## Поля манифеста

См. [`internal/manifest/schema.json`](../internal/manifest/schema.json) и раздел 4 README.

Ключевые поля `spec`:

| Поле | Назначение |
|------|------------|
| `workspace` | Путь к scenario workspace (относительно корня репозитория)
| `reference` | Путь к scenario reference (относительно корня репозитория)
| `editable` | Список файлов, которые участник может править (все остальные отклоняются)
| `spec.runtime` | Движок и ресурсы стенда; обычно `engine: compose`
| `verify.replay` | Профиль нагрузки (profile, duration, rps, concurrency)
| `verify.correctness` | Проверки correctness (система работает)
| `verify.quality` | Проверки quality (решение уместно в проде)
| `distractors` | Дистракторы — действия, маскирующие симптом, но не корень. Каждый указывает `fails:` — ID quality-check, который его ловит |

## Уровни задач

| Уровень | Что чинит участник | Пример |
|---------|-------------------|--------|
| `config` | Конфиг-файл + рестарт | log-flood-disk (`config/app.conf`)
| `code` | Исходники + Builder/Deploy | db-connection-leak (`app/handler.go`)
| `db` | SQL-выполнение | pg-table-bloat (`solution.sql`)
| `composite` | Комбинация уровней | cache-avalanche-retry-storm (config + SQL)

## Selfcheck

Команда `incidentsim selfcheck <task>`:

1. Запускает задачу (`start`)
2. Копирует reference-фикс в workspace и применяет его через `apply` — ожидает `pass`
3. Сбрасывает сбой (`reset`)
4. Для каждого дистрактора: применяет дистрактор, ожидает `partial_fix`, сбрасывает
5. Останавливает задачу (`stop`)

Все шаги должны пройти, иначе команда возвращает код 1.

Пример:

```console
$ incidentsim selfcheck db-connection-leak
=== selfcheck db-connection-leak (level=code) ===
start: PASS (session=db-connection-leak-1-120000)
apply reference: PASS
verify reference: PASS
reset: PASS
  distractor bigger-pool: Увеличить POOL_MAX в приложении... PASS (partial_fix)
  distractor restart-service: Перезапустить сервис app... PASS (partial_fix)
=== selfcheck db-connection-leak: ALL PASS ===
```

## Чеклист перед PR

- [ ] `go build ./... && go vet ./... && go test -race ./...` — зелёные
- [ ] `gofmt -l .` — пусто
- [ ] `incidentsim validate manifests` — все манифесты ok
- [ ] `incidentsim selfcheck <task>` — проходит для каждой задачи
- [ ] Новый манифест добавлен в `manifests/` и валидируется
- [ ] `scenarios/<id>/workspace/` и `scenarios/<id>/reference/` существуют (для code/config уровней)
- [ ] Дистракторы указаны в манифесте с полем `fails:`
- [ ] `reference_solution` содержит реальные шаги и команды

## Пример: добавление новой code-задачи

1. Запустите `scripts/new-task.sh <id>`; скрипт создаст code-workspace из общего stand
2. Оставьте редактируемым только `app/handler.go` (и, если нужно, миграции)
3. Создайте `manifests/<id>.yaml` с:
   - `metadata.level: code`
   - `spec.runtime.engine: compose`
   - `editable: [app/handler.go]`
   - `workspace: scenarios/<id>/workspace`
   - `reference: scenarios/<id>/reference`
   - `verify.replay` с профилем нагрузки
   - `verify.quality` с проверками, которые ловят дистракторы
   - `distractors` с `fails:` на соответствующие quality-checks
4. Добавьте `workloads/<id>.yaml` с `duration <= 30s`, `rps`, `concurrency` и
   проверками ответа; reference должен отличаться от workspace не более чем на
   40 строк и только в editable-файлах
5. Запустите `scripts/sync-stand.sh <id>` и `incidentsim validate manifests`

## Рецепт code-задачи (контракт автора)

Code-задача = общий стенд (`internal/stand`, синхронизируется
`scripts/sync-stand.sh`) + один сломанный `app/handler.go` (+ опционально
`app/migrations/*.sql`). Эталон (`reference/app/handler.go`) отличается от
workspace только в editable и не более чем на 40 строк диффа.

### Что доступно handler.go (пакет main того же каталога)

| Символ | Назначение |
|---|---|
| `db *sql.DB` | живой пул к БД (открыт до регистрации маршрутов) |
| `state *State` | честные счётчики стенда |
| `state.incRequests()`, `state.incErrors()` | счётчики запросов/ошибок |
| `state.incDBQueries()`, `state.incDepCalls()` | счётчики запросов к БД и к зависимости |
| `state.AddCacheHits(n)`, `state.SetCacheEntries(n)` | честные метрики кэша |
| `state.AddRetries(n)` | честный счётчик ретраев |
| `state.AddLogBytes(n)`, `state.SetDiskUsagePct(p)` | метрики логов/диска |
| `state.SetDBLatencyP95(d)`, `state.SetDBTableStats(dead, rows)` | метрики БД |
| `NewJobQueue(db, state)` | очередь с таблицей `jobs` (`Process`, `Enqueue`) |
| `InvariantHandler(check)` → `http.HandlerFunc` | `/debug/invariant/<name>` |
| `loadConfig()` | env + оверлей `/etc/app/app.conf` |
| `cfg.LogLevel`, `cfg.LogFile`, `cfg.PoolMax` | политики из конфига |

Метрики стенда: `app_requests_total`, `app_errors_total`,
`app_db_queries_total`, `app_db_pool_in_use`, `app_db_pool_max`,
`app_dep_calls_total`, `app_cache_entries`, `app_cache_hits_total`,
`app_retry_requests`, `app_log_bytes`, `app_disk_pct`,
`app_db_query_latency_p95_ms`, `app_db_dead_tuples`, `app_db_rows`,
`go_goroutines`, `go_heap_bytes`, `open_fds`, `uptime_seconds`.

### Стенд `dep` (зависимость)

Третий сервис `depstub` (`image: incidentsim/workload:dev`, `command:
["incidentsim-depstub"]`, порт 9090) управляется через
`POST http://depstub:9090/control` телом `{"error_rate":0.2,"latency_ms":50,
"down":false,"hang_pct":0.1}`. Приложение обращается к нему по
`http://depstub:9090/work`.

### Обязательные элементы задачи

1. Симптомы наблюдаемы без чтения кода (метрика/логи/SQL/HTTP).
2. Три подсказки с непрерывными уровнями 1..3.
3. `verify.correctness`: `/healthz` + предметная проверка «работает сейчас».
4. `verify.quality`: **артефактная** проверка (`sql`/`policy`/`log`/`exec`) +
   проверка **поведения по окну** (`metric` с `agg` + `window.from`, либо
   `replay` с `field`).
5. Дистракторы: у каждого `apply` и `fails` (id quality-проверки); `fails`
   уникальны в пределах задачи; «увеличить лимит/пул» и «перезапустить» —
   там, где они применимы.
6. Replay: `duration <= 30s`, `rps` и `concurrency` разумные; verify обязан
   укладываться в 45 с.

### Слова-спойлеры

Workspace (включая комментарии) не должен содержать отдельных слов
`bug`, `broken`, `fixme`, `leak` — `validate` и старт ядра отклоняют такой
workspace. Идентификаторы вроде `leakDetector` допустимы (границы слова).

### Проверка задачи

```console
./scripts/sync-stand.sh <id>
./bin/incidentsim validate manifests
INCIDENTSIM_ADMIN=1 ./bin/incidentsim selfcheck <id> --engine compose
```
