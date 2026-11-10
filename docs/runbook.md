# Runbook: bare-metal с KVM

Локальная разработка описана в [`local-macos.md`](local-macos.md). Здесь —
боевой путь: Linux-хост с KVM и движок Firecracker.

## Требования

* Linux x86_64 или aarch64 с включённой виртуализацией: `ls /dev/kvm` не пуст;
* Docker — только на этапе провижининга (экспорт rootfs из образа задачи);
* `mksquashfs` (squashfs-tools), `socat`;
* пайплайн не в guest: задачи не просят интернет.

## Провижининг

```console
$ ansible-galaxy collection install community.general ansible.posix
$ cp ansible/inventory.example.yml ansible/inventory.yml   # укажите хост
$ ansible-playbook -i ansible/inventory.yml ansible/firecracker.yml
```

Playbook ставит Firecracker, ядро гостя, сеть (мост + tap), systemd-юнит на VM,
экспортирует rootfs и создаёт baseline-снапшот.

## Конфигурация ядра

```yaml
engine: firecracker
catalog: manifests
state: /var/lib/incidentsim/state
firecracker:
  firecracker_bin: /usr/local/bin/firecracker
  socket_dir: /var/lib/incidentsim/vms
  kernel_image: /var/lib/incidentsim/rootfs/vmlinux
  rootfs_image: /var/lib/incidentsim/rootfs/incidentsim_workload.squashfs
  baseline_snapshot: /var/lib/incidentsim/snapshots/baseline.snap
  guest_agent_port: 51000
```

## Запуск сессии

```console
$ ./bin/incidentsim serve
$ ./bin/incidentsim start db-connection-leak
$ ./bin/incidentsim metrics <session>
$ ./bin/incidentsim reset <session>       # restore baseline-снапшота, не cold boot
$ ./bin/incidentsim stop <session>
```

## Что где

| Артефакт | Путь | Кем создаётся |
|----------|------|---------------|
| rootfs-слой | `/var/lib/incidentsim/rootfs/*.squashfs` | ansible, из образа задачи |
| baseline-снапшот | `/var/lib/incidentsim/snapshots/baseline.snap` | `incidentsim-baseline.service` |
| API-сокет VM | `/var/lib/incidentsim/vms/<session>.sock` | адаптер |
| журнал сессии | `<state>/journal/<session>.jsonl` | ядро |
| compose-проект (compose-движок) | `<state>/compose/<project>/compose.yaml` | compose-адаптер |

## Диагностика

* `incidentsim logs <session> --service app` — что видит участник;
* `journalctl -u incidentsim-vm@<session>` — жизненный цикл микро-VM;
* `curl --unix-socket /var/lib/incidentsim/vms/<session>.sock http://localhost/instance` —
  состояние VM из Firecracker API;
* `go test ./internal/adapters/firecracker/` — контракт API без KVM (тесты
  поднимают фейковый сокет).

## Известные ограничения

* microVM не имеет маршрута наружу: участник не уносит данные задачи;
* вложенная виртуализация (Firecracker внутри VM) не поддерживается —
  bare-metal или ничего;
* `exec` и метрики идут через vsock-агент `incidentsim-guest`; если агент не
  поднялся, `Up` отвечает `guest agent did not come up`.
