# Runbook

Для разработки IncidentSim запускается через Docker Compose. Требуются Go 1.24+
и Docker Compose v2.

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

## Firecracker на Linux/KVM

Firecracker — отдельный продуктовый движок, одна microVM на сессию. На хосте
должны быть `/dev/kvm`, `mkfs.ext4`, `mksquashfs` и Docker для экспорта rootfs.
Провижининг выполняется так:

```console
ansible-playbook -i ansible/inventory.example.yml ansible/firecracker.yml
INCIDENTSIM_CONFIG=incidentsim-firecracker.yaml bin/incidentsim serve
bin/incidentsim selfcheck db-connection-leak
```

На реальном KVM-хосте этот checkout не проверен. Если KVM или Firecracker
отсутствует, адаптер возвращает `ErrUnsupported` с подсказкой использовать
Compose.
