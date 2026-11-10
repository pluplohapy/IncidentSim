# Как добавить задачу

Новый манифест в [`manifests/`](../manifests) — новая задача. Правка ядра не
нужна; если кажется, что нужна, значит меняется схема (см.
[`skills/spec-first/SKILL.md`](../skills/spec-first/SKILL.md)).

## Шаги

1. **Определи корень и его слой.** `metadata.level`: `config`, `code`, `db`,
   `composite`.
2. **Опиши окружение.** `spec.services`: образы, env, порты, healthcheck.
   Образ задачи = ваш учебный сервис, он же rootfs в микро-VM.
3. **Опиши сбой.** `spec.fault` (или `spec.fault_chain` для composite).
   Сбой живёт в инъекторе, а не в env сервиса: окружение стартует здоровым,
   сбой активируется после `Up`.
4. **Опиши симптомы и подсказки.** Что видит участник; подсказки по уровням.
5. **Опиши `verify.correctness`.** Поведение: http-статус, метрика в окне.
6. **Опиши `verify.quality`.** Артефакты: policy, sql, log. Правило: хотя бы
   одна артефактная проверка.
7. **Опиши дистрактор.** `masked_artifact` + `fails` (id quality-проверки).
   Дистрактор без проверки, которая его ловит, — не дистрактор, а баг.
8. **Добавь replay-профиль** в [`workloads/`](../workloads). Без нагрузки
   задачу решает останов сервиса.
9. **Проверь:**

```console
$ make check                                            # тесты ядра
$ ./bin/incidentsim validate                            # схема + кросс-ссылки
$ ./scripts/demo.sh <id>                                # оба шлюза на живой среде
```

## Минимальный манифест

```yaml
apiVersion: incidentsim/v1
kind: Incident
metadata:
  id: my-task
  title: "Что сломалось с точки зрения участника"
  level: config
spec:
  runtime:
    engine: compose
  services:
    - name: app
      image: incidentsim/workload:dev
      ports: [8080]
      healthcheck:
        path: /healthz
        port: 8080
  fault:
    type: feature_flag
    target: app
    options:
      env:
        LOG_LEVEL: debug
  verify:
    correctness:
      - id: healthz-200
        type: http
        service: app
        path: /healthz
        expect: { status: 200 }
    quality:
      - id: log-level-info
        type: policy
        service: app
        rule: env_flag_equals
        query: LOG_LEVEL
        expect: { equals: info }
        reason: "debug-логи в проде запрещены политикой"
  root_cause:
    summary: "Что на самом деле сломано и почему."
    layer: config
    signals: ["чем это подтверждается"]
  reference_solution:
    summary: "Что должен сделать участник."
    steps: ["шаг 1", "шаг 2"]
  replay:
    profile: workloads/my-task.yaml
    duration: 30s
```

## Проверка своей задачи на смысл

Прогони [`skills/verifier-design/SKILL.md`](../skills/verifier-design/SKILL.md):
для каждой quality-проверки назови дистрактор, которого она ловит. Не назвал —
проверка декоративная.
