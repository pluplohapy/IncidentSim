#!/usr/bin/env bash
#
# Собирает весь реализованный код проекта (кроме библиотечного/стороннего
# кода и артефактов сборки) и все README-файлы в один output.txt.
#
# Каждый файл в выводе предваряется заголовком FILE: <путь>.
#
# Использование:
#   ./scripts/collect_code.sh [output.txt]
#
# По умолчанию создаёт ./output.txt в корне репозитория.
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

OUT="${1:-output.txt}"

# Директории, которые никогда не считаются кодом проекта.
EXCLUDE_DIRS=(.git .idea .vscode .incidentsim bin dist vendor node_modules)
# Имена файлов, которые исключаются из списка (в т.ч. предыдущий запуск скрипта).
EXCLUDE_BASENAMES=(output.txt)

# Является файл собственным (не библиотечным) кодом проекта.
is_code_file() {
    local f="$1" base
    base="$(basename "$f")"

    case "$base" in
        Makefile|makefile|GNUmakefile) return 0 ;;
        go.mod|go.sum) return 0 ;;
        Dockerfile|dockerfile|*.Dockerfile|*.dockerfile) return 0 ;;
    esac

    case "$f" in
        *.go|*.sh|*.bash|*.yml|*.yaml|*.json|*.conf|*.sql|*.md) return 0 ;;
    esac

    return 1
}

# Является ли файл README-файлом (любой вариант имени/расширения).
is_readme() {
    local base
    base="$(basename "$1")"
    case "$base" in
        README*|Readme*|readme*) return 0 ;;
    esac
    return 1
}

# Список файлов: отслеживаемые git + новые, не проигнорированные .gitignore.
mapfile -t ALL_FILES < <(git ls-files --cached --others --exclude-standard | sort)

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

: > "$tmp"

for f in "${ALL_FILES[@]}"; do
    # Пропускаем файлы внутри исключённых директорий.
    skip=0
    old_ifs="$IFS"
    IFS='/'
    # shellcheck disable=SC2206
    parts=($f)
    IFS="$old_ifs"
    for p in "${parts[@]}"; do
        for ex in "${EXCLUDE_DIRS[@]}"; do
            if [[ "$p" == "$ex" ]]; then skip=1; break 2; fi
        done
    done
    [[ "$skip" == 1 ]] && continue

    base="$(basename "$f")"
    for ex in "${EXCLUDE_BASENAMES[@]}"; do
        if [[ "$base" == "$ex" ]]; then skip=1; break; fi
    done
    [[ "$skip" == 1 ]] && continue

    if is_code_file "$f" || is_readme "$f"; then
        printf '%s\n' "$f" >> "$tmp"
    fi
done

# Уникализируем и сортируем итоговый список.
mapfile -t FILES < <(sort -u "$tmp")

{
    echo "IncidentSim — собранный код и README-файлы"
    echo "Сформировано: $(date '+%Y-%m-%d %H:%M:%S %z')"
    echo "Корень проекта: $ROOT"
    echo "Всего файлов: ${#FILES[@]}"
    echo
    echo "=================================================================="
    echo

    for f in "${FILES[@]}"; do
        [[ -f "$f" ]] || continue
        echo "------------------------------------------------------------------"
        echo "FILE: $f"
        echo "------------------------------------------------------------------"
        cat -- "$f"
        echo
        echo
    done
} > "$OUT"

echo "Готово: $OUT (${#FILES[@]} файлов)"
