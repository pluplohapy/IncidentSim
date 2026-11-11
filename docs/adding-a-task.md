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
