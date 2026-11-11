# Локальный запуск на macOS

IncidentSim использует только Docker Compose. Подойдёт Docker Desktop или
Colima с Compose v2.

```console
brew install colima docker docker-compose
colima start --cpu 4 --memory 8 --disk 60 --vm-type vz
make build
bin/incidentsim serve
```

В другом терминале используйте `bin/incidentsim start <task>`. Endpoint
сессии и путь workspace выводятся командой start/status; их не нужно угадывать.
