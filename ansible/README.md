# ansible/

Провижининг bare-metal хоста под движок Firecracker.

## Что делает каждая часть

| Файл | Зачем |
|------|-------|
| [`firecracker.yml`](firecracker.yml) | главный playbook: установка, сеть, systemd и rootfs |
| [`inventory.example.yml`](inventory.example.yml) | пример инвентаря bare-metal хоста |
| `roles/firecracker/tasks/main.yml` | мост и tap-устройства |
| `roles/firecracker/defaults/main.yml` | пути; они же, что в `incidentsim.yaml` |
| `roles/firecracker/templates/` | сеть, core и per-VM systemd-юниты |

## Запуск

```console
$ ansible-galaxy collection install community.general ansible.posix
$ ansible-playbook -i inventory.example.yml ansible/firecracker.yml
```

После провижининга в `incidentsim.yaml` включается движок:

```yaml
engine: firecracker
firecracker:
  firecracker_bin: /usr/local/bin/firecracker
  socket_dir: /var/lib/incidentsim/vms
  kernel_image: /var/lib/incidentsim/vmlinux
  rootfs_dir: /var/lib/incidentsim/rootfs
  version: 1.11.0
  guest_agent_port: 51000
```

## Требования к хосту

* Linux x86_64 или aarch64 с включённой виртуализацией (`/dev/kvm` есть);
* Docker — только на этапе провижининга, для экспорта rootfs из образа
  workload-сервиса;
* `mksquashfs` (squashfs-tools), `socat`.

На macOS этого пути нет: `/dev/kvm` не существует, и эмуляция не
поддерживается. Локальная разработка идёт через compose-движок
([`docs/local-macos.md`](../docs/local-macos.md)).

## Сеть

Мост `incbr0` с адресом 172.30.0.1/24, tap-устройства `incidentsim-tap0..N`.
У гостя нет маршрута наружу: участник не может унести данные задачи или
достать интернет из тренажёра. Хост управляет гостем через vsock-агент
(`incidentsim-guest`), а HTTP-проверки идут по закрытой host-only сети на
фиксированный IP гостя.

## Reset

`incidentsim reset` пересоздаёт writable overlay и перезагружает microVM. Это
MVP-путь без snapshot, потому что snapshot Firecracker связан с исходными
дисками, tap и vsock-сокетами.

Роль также ставит `incidentsim-core.service`. Перед включением юнита положите
собранный `incidentsim` в путь `incidentsim_core_bin` и конфигурацию в
`incidentsim_core_config`.
