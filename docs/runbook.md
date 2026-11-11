# Runbook

IncidentSim запускается через Docker Compose. Требуются Go 1.24+ и Docker
Compose v2.

```console
make build
bin/incidentsim serve --config incidentsim.yaml
bin/incidentsim start db-connection-leak
bin/incidentsim verify <session>
bin/incidentsim stop <session>
```

Журналы находятся в `<state>/journal`, а workspace активной сессии — в
`<state>/sessions/<session>/workspace`. `stop` удаляет workspace, compose
проект и образ code-задачи.
