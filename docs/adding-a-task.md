# Добавление новой задачи в IncidentSim

## Структура задачи

Каждая задача живет в `scenarios/<id>/` и содержит:

```
scenarios/<id>/
├── workspace/              # разбитая рабочая копия (копируется в сессию при start)
│   └── ...                 # файлы, которые участник может править
├── reference/              # эталонное решение (используется selfcheck)
│   └── ...                 # reference-файлы (app/main.go, config/app.conf, solution.sql)
└── (опционально) Dockerfile, go.mod, go.sum
```

## Поля манифеста

См. [`internal/manifest/schema.json`](../internal/manifest/schema.json) и раздел 4 README.

Ключевые поля `spec`:

| Поле | Назначение |
|------|------------|
| `workspace` | Путь к scenario workspace (относительно корня репозитория)
| `reference` | Путь к scenario reference (относительно корня репозитория)
| `editable` | Список файлов, которые участник может править (все остальные отклоняются)
| `verify.replay` | Профиль нагрузки (profile, duration, rps, target_service)
| `verify.correctness` | Проверки correcteness (система работает)
| `verify.quality` | Проверки quality (решение уместно в проде)
| `distractors` | Дистракторы — действия, маскирующие симптом, но не корень. Каждый указывает `fails:` — ID quality-check, который его ловит |

## Уровни задач

| Уровень | Что чинит участник | Пример |
|---------|-------------------|--------|
| `config` | Конфиг-файл + рестарт | log-flood-disk (config/app.conf)
| `code` | Исходники + пересборка образа | db-connection-leak (app/main.go)
| `db` | SQL-выполнение | pg-table-bloat (ALTER TABLE, VACUUM)
| `composite` | Комбинация уровней | cache-avalanche-retry-storm (config + SQL)

## Selfcheck

Команда `incidentsim selfcheck <task>`:

1. Запускает задачу (`start`)
2. Применяет reference-фикс (`apply`) — ожидает `pass`
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

## Пример: добавление новой config-задачи

1. Создайте `scenarios/<id>/workspace/config/app.conf` с разбитной конфигурацией
2. Создайте `scenarios/<id>/reference/config/app.conf` с исправленной конфигурацией
3. Создайте `manifests/<id>.yaml` с:
   - `level: config`
   - `editable: [config/app.conf]`
   - `workspace: scenarios/<id>/workspace`
   - `reference: scenarios/<id>/reference`
   - `verify.replay` с профилем нагрузки
   - `verify.quality` с проверками, которые ловят дистракторы
   - `distractors` с `fails:` на соответствующие quality-checks
4. Запустите `incidentsim selfcheck <id>` для проверки
