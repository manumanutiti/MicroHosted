# Despliegue como servicio systemd

El daemon **no debe correrse en primer plano** en una terminal interactiva:
un Ctrl-C manda SIGINT a todo el grupo de procesos de la terminal, lo que mata
también los procesos Firecracker hijos y tira las microVMs. Como servicio
systemd no hay terminal de control, y la unit está configurada para que las
VMs sobrevivan a los reinicios del daemon.

## Instalar

```bash
make install-service                 # compila (como tu usuario) e instala la unit
sudo systemctl enable --now microhosted
systemctl status microhosted
```

Dirección de escucha distinta:

```bash
make install-service ADDR=127.0.0.1:9000
```

## Operar

```bash
journalctl -u microhosted -f         # logs en vivo
sudo systemctl restart microhosted   # reinicia el daemon SIN matar las VMs vivas
sudo systemctl stop microhosted      # para el daemon; las VMs siguen corriendo
```

Tras un `restart` o un arranque, en el log verás `reconcile: adopted running
vm <id>` por cada VM que seguía viva, o `reconcile: swept dead vm <id>` por las
que hubieran muerto mientras el daemon estaba parado.

## Por qué la unit es como es

- **`KillMode=process`** — systemd solo señala al proceso principal (el
  orquestador), nunca a los Firecracker hijos. Es lo que permite que
  `Manager.Reconcile` readopte las VMs por PID al volver a arrancar.
- **`Delegate=yes`** — Jailer crea un cgroup por VM; delegar el subárbol de
  cgroup al servicio evita chocar con la gestión de systemd.
- **`AssertPathExists=/dev/kvm`** — sin KVM el servicio falla claro al arrancar,
  no más tarde con un error oscuro.
- Corre como **root**: Jailer necesita crear chroots/cgroups/tap y abrir
  `/dev/kvm`.

## Desinstalar

```bash
make uninstall-service               # para y quita el servicio (no toca las VMs)
```
