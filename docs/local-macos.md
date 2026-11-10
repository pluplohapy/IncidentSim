# Локальный запуск на macOS (Apple Silicon)

Firecracker требует Linux и KVM. На macOS `/dev/kvm` не существует, и эмуляция
не поддерживается: эмулятор замедляет микро-VM настолько, что задача перестаёт
быть задачей. Поэтому локальный путь — **Colima + Docker Compose**, а
Firecracker-движок работает на bare-metal Linux (см.
[`runbook.md`](runbook.md)).

## Почему Colima

Colima поднимает Linux-VM через Virtualization.framework (он же `vz`), то есть
без KVM и без отдельного гипервизора, и даёт настоящий Docker внутри этой VM.
Другие варианты:

| Вариант | Почему не он |
|---------|--------------|
| Docker Desktop | работает, но тянет GUI и лицензию; Colima делает то же самое из терминала |
| UTM | эмулятор: микро-VM Firecracker внутри UTM не получит KVM |
| Firecracker внутри Lima/mvm | тот же барьер: вложенной виртуализации на Apple Silicon нет |
| «Починить KVM на маке» | нельзя: `/dev/kvm` — часть ядра Linux, на macOS его нет |

Эмуляция nested virtualization на Apple silicon отсутствует на уровне
аппаратуры, поэтому это не настройка, а ограничение платформы.

## Одна команда

```console
$ ./scripts/demo.sh
```

Скрипт сам: проверит окружение, поднимет ядро, соберёт образ задачи, запустит
задачу, применит дистрактор, покажет два шлюза и постмортем.

## Ручная цепочка

```console
# 1. Linux-VM с Docker (один раз)
$ brew install colima docker docker-compose
$ mkdir -p ~/.docker && cat > ~/.docker/config.json <<'JSON'
{
  "cliPluginsExtraDirs": [
    "/opt/homebrew/lib/docker/cli-plugins"
  ]
}
JSON
$ colima start --cpu 4 --memory 8 --disk 60 --vm-type vz

# 2. бинарники и образ задачи
$ make build
$ make workload-image

# 3. ядро
$ ./bin/incidentsim serve          # или: make run-dev

# 4. задача
$ ./bin/incidentsim show
$ ./bin/incidentsim start db-connection-leak
$ ./bin/incidentsim metrics <session>
$ ./bin/incidentsim verify <session>
$ ./bin/incidentsim postmortem <session>
```

## Что проверено на этой машине

Intel-подобная проверка не делалась: разработка велась на arm64
(`go version go1.27.3 darwin/arm64`, colima `aarch64`, образ
`incidentsim/workload:dev` собирается под arm64 и работает внутри VM).

## Ограничения локального пути

* движок `firecracker` недоступен: `Up` отвечает
  `operation unsupported by this runtime: /dev/kvm is missing`;
* порты публикуются эфемерно: endpoint сессии берётся из
  `curl http://127.0.0.1:8747/api/v1/sessions/<id>`, а не угадывается;
* сессии живут в памяти ядра: перезапуск `incidentsim serve` теряет список
  сессий, но журналы остаются в `.incidentsim/journal/` и постмортем по ним
  строится;
* образ `postgres:16-alpine` тянется из интернета при первом старте задачи с БД.
