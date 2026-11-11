# Архитектура

Спецификация — [`../README.md`](../README.md). Здесь — диаграммы и связи,
которые в тексте спецификации разъехались бы.

## Компоненты

```mermaid
flowchart TB
    subgraph host["Хост (macOS или bare-metal)"]
        CLI["incidentsim CLI<br/>show / start / metrics / logs / hint / apply / verify / reset / postmortem"]
        CORE["core<br/>HTTP API, оркестратор, журнал, постмортем"]
        CAT["catalog<br/>manifests/*.yaml"]
        CLI -->|HTTP :8747| CORE
        CORE --> CAT
        CORE --> INJ["injector<br/>feature_flag, sql_config, kill"]
        CORE --> VER["verifier<br/>replay, checks, policy, два gate"]
        INJ --> RT["runtime.Adapter"]
        VER --> RT
        CORE --> JRN["journal<br/>.incidentsim/journal/*.jsonl"]
        CORE --> PM["postmortem<br/>из journal"]
    end
    subgraph engines["Движок изоляции"]
        RT --> COMPOSE["compose adapter<br/>docker compose"]
    end
    COMPOSE --> DOCKER["Docker daemon"]
    VER --> LOAD["replay runner<br/>нагрузка в окне наблюдения"]
```

## Жизненный цикл сессии

```mermaid
sequenceDiagram
    participant U as Участник
    participant A as API ядра
    participant O as Оркестратор
    participant R as Runtime
    participant I as Инъектор
    participant V as Верификатор

    U->>A: start <task>
    A->>O: Start
    O->>R: Up(spec)
    R-->>O: Instance
    O->>I: Activate(fault...)
    I->>R: Apply(env / exec)
    O-->>U: сессия running

    U->>A: apply из workspace
    A->>O: Apply
    O->>R: Apply(patch)
    O-->>U: применено

    U->>A: verify
    A->>V: Verify(replay + checks)
    V->>R: metrics, logs, sql, env
    V-->>A: Report(correctness, quality)
    A-->>U: pass/fail + детализация

    U->>A: reset
    A->>O: Reset
    O->>R: Reset (down/up)
    O->>I: Activate(fault...) снова
    O-->>U: baseline восстановлен
```

## Два шлюза

```mermaid
flowchart LR
    subgraph checks["Проверки манифеста"]
        C["correctness<br/>http, metric, log, exec"]
        Q["quality<br/>policy, sql, log, exec"]
    end
    C -->|все pass| GC["correctness gate: pass"]
    C -->|есть fail| FC["correctness gate: fail"]
    Q -->|все pass| GQ["quality gate: pass"]
    Q -->|есть fail| FQ["quality gate: fail"]
    GC -->|и quality pass| P["verdict: PASS"]
    GQ -->|и correctness pass| P
    FC --> F["verdict: FAIL"]
    FQ --> F
    P -.->|обычный исход| OK["задача решена"]
    F -.->|"correctness pass + quality fail"| PARTIAL["ЧАСТИЧНЫЙ ФИКС:<br/>симптом устранён, корень остался"]
```

Ключевая точка на схеме: красный `quality` при зелёном `correctness` — это не
ошибка проверки, а главный результат симулятора.
