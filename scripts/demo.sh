#!/usr/bin/env bash
#
# Демо одной задачи от start до postmortem на локальной машине.
#
# Скрипт показывает то, ради чего существует симулятор: систему можно починить
# двумя способами, и только один из них проходит оба шлюза.
#
#   ./scripts/demo.sh                       # задача по умолчанию
#   ./scripts/demo.sh cache-avalanche-retry-storm
#
set -euo pipefail

TASK="${1:-db-connection-leak}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

ADDR="${INCIDENTSIM_ADDR:-127.0.0.1:8747}"
STATE="$ROOT/.incidentsim"

say() { printf '\n=== %s ===\n' "$*"; }

# --- 1. окружение -----------------------------------------------------------
say "проверка окружения"
if ! command -v docker >/dev/null 2>&1; then
  echo "docker не найден. На macOS запустите colima: colima start --cpu 4 --memory 8 --disk 60"
  echo "Подробнее: docs/local-macos.md"
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "демон Docker не отвечает. Запустите colima (или Docker Desktop) и повторите."
  echo "Подробнее: docs/local-macos.md"
  exit 1
fi
echo "docker готов: $(docker info --format '{{.OperatingSystem}} {{.Architecture}}')"

# --- 2. сборка --------------------------------------------------------------
say "сборка ядра, CLI и образа задачи"
make build >/dev/null
docker build -q -t incidentsim/workload:dev -f build/workload.Dockerfile . >/dev/null
echo "образ incidentsim/workload:dev собран"

# --- 3. ядро ----------------------------------------------------------------
say "запуск ядра"
if curl -fsS "http://$ADDR/healthz" >/dev/null 2>&1; then
  echo "ядро уже работает на $ADDR"
else
  ./bin/incidentsim serve >"$STATE/demo-serve.log" 2>&1 &
  mkdir -p "$STATE"
  echo $! >"$STATE/demo-serve.pid"
  for _ in $(seq 1 50); do
    curl -fsS "http://$ADDR/healthz" >/dev/null 2>&1 && break
    sleep 0.2
  done
  echo "ядро слушает $ADDR (лог: $STATE/demo-serve.log)"
fi

session() { ./bin/incidentsim --version >/dev/null; }

# --- 4. задача --------------------------------------------------------------
say "старт задачи $TASK"
SESSION="$(./bin/incidentsim start "$TASK" 2>/dev/null | head -1 | sed 's/^session \([^ ]*\).*/\1/')"
echo "сессия: $SESSION"

say "подсказка"
./bin/incidentsim hint "$SESSION" || true

say "первая проверка: окружение сломано"
./bin/incidentsim verify "$SESSION" || true

say "убираем симптом, не трогая причину"
case "$TASK" in
  log-flood-disk)
    ENDPOINT="$(curl -s "http://$ADDR/api/v1/sessions/$SESSION" \
      | python3 -c 'import sys,json;print(json.load(sys.stdin)["session"]["services"][0].split("(")[1].rstrip(")"))')"
    curl -fsS -X POST "http://$ENDPOINT/debug/clear-logs" >/dev/null
    echo "лог-файл очищен, уровень логирования всё ещё debug"
    ;;
  db-connection-leak)
    ./bin/incidentsim apply "$SESSION" --service db --sql "ALTER SYSTEM SET max_connections = 500" >/dev/null
    docker compose --project-name "incidentsim-$SESSION" restart db >/dev/null
    echo "лимит соединений БД раздут, утечка в приложении осталась"
    ;;
  pg-table-bloat)
    ./bin/incidentsim apply "$SESSION" --service db --sql "TRUNCATE orders" >/dev/null
    echo "таблица очищена, autovacuum отключён и данные потеряны"
    ;;
  cache-avalanche-retry-storm)
    ./bin/incidentsim apply "$SESSION" --service app --set RETRY_ENABLED=false >/dev/null
    echo "ретраи отключены: storm исчез, устойчивость тоже"
    ;;
esac

say "вторая проверка: симптом устранён, корень остался"
./bin/incidentsim verify "$SESSION" || true

say "постмортем сессии"
./bin/incidentsim postmortem "$SESSION" || true

say "уборка"
./bin/incidentsim stop "$SESSION" || true

say "итог"
echo "Correctness отвечает на вопрос 'система работает сейчас'."
echo "Quality отвечает на вопрос 'решение уместно в проде'."
echo "В демо оба раза система отвечала — и оба раза прогон был красным."
