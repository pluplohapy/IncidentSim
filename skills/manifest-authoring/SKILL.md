---
name: manifest-authoring
description: Правила написания YAML-манифеста задачи IncidentSim: обязательные поля, editable, fault_chain, root_cause, distractors, verify.correctness/quality.
---

# SKILL: manifest-authoring

Схема — [`internal/manifest/schema.json`](internal/manifest/schema.json), типы —
[`internal/manifest/manifest.go`](internal/manifest/manifest.go), семантические
правила — [`internal/manifest/loader.go`](internal/manifest/loader.go). Скилл
объясняет *зачем* поля устроены так; дублирование схемы здесь не нужно.

## Порядок написания новой задачи

1. **Определи корневую причину и её слой** (`metadata.level`):
   `config` — неверная конфигурация, `code` — дефект в коде, `db` — состояние
   базы, `composite` — цепочка сбоев.
2. **Опиши симптомы** (`spec.symptoms`): что видит участник до диагностики.
   Симптом — это наблюдение, а не причина («503 на /api/orders», а не «утечка»).
3. **Опиши сбой** (`spec.fault` или `spec.fault_chain`) и его `trigger`:
   `on_start` по умолчанию, `after_30s` для сбоя, который нарастает,
   `manual` — активируется инструктором.
4. **Опиши distractors** (`spec.distractors`): действия, которые убирают симптом,
   но не причину. Каждый обязан иметь `fails` — id quality-проверки, которая его
   ловит. Нет проверки — дистрактор нельзя добавить (схема и загрузчик это
   проверяют).
5. **Опиши `verify.correctness`** — поведенческие проверки «система работает
   сейчас»: http-статус, метрика в окне, SQL-факт.
6. **Опиши `verify.quality`** — проверки «решение уместно в проде». Обязательно
   хотя бы одна `policy`-проверка с `reason`, объясняющим производственную
   ценность. Без policy-проверки маскировка симптома проходит задачу.
7. **Заполни `root_cause`**: `summary`, `layer`, `signals` (артефакты,
   подтверждающие причину), `masked_by` (id дистракторов).
8. **Заполни `reference_solution`**: `summary`, `steps`, `commands`, `rollback`.
9. **Добавь `replay`-профиль** в [`workloads/`](../workloads) — без нагрузки
   задача решается простоем сервиса.

## Обязательные поля

| Поле | Зачем | Что будет, если забыть |
|------|-------|------------------------|
| `metadata.id` | имя в каталоге и в CLI | загрузчик отвергнет manifest (должно совпадать с именем файла) |
| `metadata.level` | выбирает тип проверок | значение по умолчанию `config` подменит замысел |
| `spec.services[].image` | из чего собирается окружение | задача нестартуема |
| `spec.fault.type` | какой инъектор активировать | «either fault or fault_chain must be set» |
| `spec.verify.correctness` | шлюз «работает» | задача не проверяема |
| `spec.verify.quality` | шлюз «правильно» | «работает = правильно», главный принцип нарушен |
| `spec.root_cause.summary` | сверка в постмортеме | постмортем без причины |
| `spec.distractors[].fails` | связь маскировки с проверкой | «must reference a quality check id» |

## Правила, которые проверяются автоматически

* `apiVersion: incidentsim/v1`, `kind: Incident` — иначе отказ.
* Неизвестные поля отвергаются схемой (`additionalProperties: false`).
* `fault.target` и `check.service` должны ссылаться на объявленный сервис.
* `fault_chain` требует `metadata.level: composite` и не менее двух звеньев
  с уникальными типами.
* `quality` должен содержать policy-проверку; каждая проверка типа `policy`
  должна объяснить `reason`.
* `distractors` требуют `symptoms` и ссылки на существующий id.
* `replay.profile` должен существовать (проверка `ValidateWithAssets`).

## Чего не делать

* Не добавляй поле «на всякий случай»: схема фиксирована, новые концепции —
  отдельным изменением схемы с ревью.
* Не пиши качественные проверки типом `http`: 200 на `/healthz` не отличает
  лечение от маскировки.
* Не описывай дистрактор без `masked_artifact`: непонятно, что именно
  уничтожается.
* Не перечисляй в `reference_solution` действия, которые сами нарушают
  `quality` (например «удалить логи»).

## Как проверить скилл

```console
$ go test ./internal/manifest/            # негативные кейсы схемы и загрузчика
$ bin/incidentsim show --validate         # валидация всего каталога
```

Возьми новый манифест, удали из него по очереди `spec.verify.quality`,
`spec.distractors[].fails` и `spec.root_cause.summary` — каждый раз загрузчик
должен отказать работу с внятной ошибкой.

Связанные: [spec-first](spec-first/SKILL.md),
[verifier-design](verifier-design/SKILL.md), [docs](docs/SKILL.md).
