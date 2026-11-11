---
name: firecracker-ansible
description: Как поднимать task-microVM через Ansible, работать с tap-сетью, снапшотами и systemd-юнитами.
---

# SKILL: firecracker-ansible

Playbook — [`ansible/firecracker.yml`](../ansible/firecracker.yml), роли и
шаблоны — [`ansible/roles/firecracker/`](../ansible/roles/firecracker/),
адаптер — [`internal/adapters/firecracker/`](../internal/adapters/firecracker/).
Скилл объясняет *почему* устроено так; конкретные пути не дублируются.

## Когда применять

* изменения в `ansible/` или в `internal/adapters/firecracker/`;
* новая задача требует другого rootfs или другой конфигурации гостя;
* хост переносится на другой дистрибутив или архитектуру.

## Порядок

1. **Проверь, что хост вообще может.** `/dev/kvm` существует, `kvm_intel` или
   `kvm_amd` загружен, firecracker запускается. На macOS этого нет и быть не
   может — не пытайся эмулировать KVM, работай на compose-движке.
2. **Сеть до сервисов.** Мост + tap-устройства. У гостя нет маршрута наружу:
   участник не должен унести данные задачи или достать интернет из тренажёра.
   Хост общается с гостем через vsock-агент, не через сеть.
3. **systemd после сети.** Юнит на VM создаётся из шаблона
   `incidentsim-vm@.service`, `%i` — session id. Jailed-исполнение: свой
   пользователь, `DeviceAllow=/dev/kvm rw`, `ReadWritePaths` только на
   артефакты.
4. **Rootfs из образа.** `docker create` → `docker export` → `mksquashfs`.
   Один образ задач = один read-only слой; overlay пишется отдельно на VM.
   Это мост между compose-миром и microVM-миром.
5. **Baseline-снапшот последним.** Он один раз грузит гостя и делает
   `snapshot/create`. Всё, что нужно после — `snapshot/resume`.

## Инварианты

* адаптер и Ansible не расходятся в путях: значения
  `roles/firecracker/defaults/main.yml` совпадают с полями
  `Config.Firecracker*` в [`internal/core/config.go`](../internal/core/config.go);
* базовый слой rootfs read-only, overlay writable;
* reset всегда restore, никогда cold boot — иначе reset перестаёт быть быстрым;
* неизвестный fault type не активен и старт сессии падает, а не тихо
  поднимается здоровым.

## Чего не делать

* не пробрасывать сетевой мост в гостя с маршрутом по умолчанию;
* не хранить секреты задач в снапшоте или в аргументах командной строки
  (они видны в `ps`);
* не запускать firecracker от root без jailer'а и cgroup-ограничений.

## Как проверить скилл

```console
$ ansible-playbook --syntax-check ansible/firecracker.yml
$ ansible-lint ansible/ || true     # стиль, не блокирует
$ go test ./internal/adapters/firecracker/   # контракт API без KVM
```

На хосте с KVM: `incidentsim start db-connection-leak` → `incidentsim reset`
должен возвращать инцидент, а `incidentsim verify` после фикса — проходить оба
шлюза.

Связанные: [manifest-authoring](manifest-authoring/SKILL.md),
[code-review](code-review/SKILL.md), [docs](docs/SKILL.md).
