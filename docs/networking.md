# Red segmentada (Fase 1)

Reemplaza el modelo `/30` punto-a-punto (donde el host era gateway de cada VM y
dos VMs no podían verse) por **redes con nombre**, que resuelven a la vez
"redes entre máquinas" y "aislamiento contra maliciosos" con un solo mecanismo.

## Modelo

Una **Network** es un segmento L2 con nombre:

- un **bridge** Linux (`mhbr<id>`, donde `<id>` son 8 hex — cabe en los 15
  chars de `IFNAMSIZ`),
- una **subred** (CIDR, p.ej. `172.16.0.0/24`),
- un **gateway** = IP del host en el bridge (`.1` de la subred),
- un flag **egress** (salida a internet sí/no) y opcionalmente **allowed_egress**
  (egress de grano fino),
- un flag **intra** (conectividad VM↔VM dentro de la red; **off por defecto**).

Cada VM que entra a una red recibe un **TAP enslavado a ese bridge** (el TAP no
lleva IP; la IP de gateway vive en el bridge) y una **IP de la subred** de la
red (IPAM por-red). El gateway del guest es el del bridge.

- VMs en la **misma** red → **aisladas por defecto** (deny-by-default): cada
  TAP se enslava como *isolated bridge port* del kernel, que no intercambia
  tramas con otros puertos aislados pero sí con el bridge (el gateway). Solo
  con `intra: true` la red es un segmento L2 clásico donde las VMs se ven.
  Esto se hace a nivel L2 a propósito: el tráfico same-bridge **no pasa por
  nftables** (es switching puro), una regla de forward no podría cortarlo.
- VMs en redes **distintas** → aisladas (bridges separados + regla nftables que
  descarta el tráfico cruzado).
- `no_network: true` sigue válido: VM sin TAP, sin IP, solo vsock. El sandbox
  más hermético.

## Política nftables (tabla `inet microhosted`)

- **guest → host**: DROP. La VM no puede alcanzar servicios del host (solo su
  gateway, y solo para enrutar si hay egress).
- **cross-segment**: DROP. El tráfico de la subred A hacia la subred B se
  descarta. Implementado como UNA regla agregada sobre sets (`@mhbridges` +
  exención `@mhsame` para el tráfico intra-bridge bajo br_netfilter), no una
  regla por par de bridges: el ruleset se mantiene O(N) con N redes, clave
  para la topología red-por-VM.
- **egress**:
  - `egress: true`  → `MASQUERADE` de la subred por la interfaz de salida por
    defecto + FORWARD permitido hacia la WAN.
  - `egress: false` → sin NAT y FORWARD hacia la WAN descartado. **Default**,
    por seguridad (malware-safe): una muestra no llama a casa salvo que se pida
    explícitamente.
  - `egress: false` + **`allowed_egress`** → egress de grano fino: solo los
    flujos listados (IP/CIDR destino + protocolo + puerto) se aceptan en
    `forward` **antes** del drop hacia la WAN, y la subred se enmascara para
    que esos flujos tengan NAT. Todo lo demás sigue cayendo en el drop. Caso
    típico IoT: un parser que solo puede hablar con su broker MQTT
    (`203.0.113.7:8883/tcp`) y nada más.

## Egress de grano fino (`allowed_egress`)

Cada regla es `{ip, protocol, port}`: `ip` es una IPv4 o CIDR IPv4 **en forma
canónica**, `protocol` ∈ {`tcp`, `udp`, `icmp`} (minúsculas), y `port`
(1–65535) es obligatorio para tcp/udp y prohibido para icmp. Es **mutuamente
exclusivo** con `egress: true` (que ya lo permite todo).

La validación (`ValidateEgressRules`) es una frontera de seguridad, no una
comodidad: los campos se interpolan tal cual en el script `nft -f`, así que
cualquier cosa que no sea estrictamente IPv4 canónica + protocolo conocido +
puerto numérico se rechaza en `POST /v1/networks` — si no, sería inyección de
ruleset.

En el `forward` chain el orden por red restringida es: accepts de
`allowed_egress` → drop hacia la WAN. En `postrouting`, la subred restringida
se enmascara entera — es seguro porque postrouting solo ve paquetes que el
`forward` chain ya aceptó.

### Update en caliente de intra (`PUT /v1/networks/{name}/intra`)

El flag `intra` también se puede cambiar en vivo: se persiste y luego
`vm.Manager.SyncTapIsolation` recorre los TAPs vivos de la red re-aplicando
`bridge link set ... isolated on/off`. El reparto de responsabilidades es
deliberado: el flag vive en el manager de red, pero los nombres de TAP
pertenecen a las VMs (un fork en Firecracker < 1.12 puede reutilizar el TAP
del snapshot, así que `tap<id>` no es una convención fiable) — por eso el
recorrido lo hace el manager de VMs. Si algún TAP no converge al endurecer,
el endpoint devuelve 500 nombrándolo: un TAP que conserva alcanzabilidad
antigua es un agujero, no un detalle de log.

### Update en caliente de egress (`PUT /v1/networks/{name}/egress`)

La política de egress de una red viva se puede **reemplazar** sin tocar las
VMs conectadas: bridge, subred e IPs no cambian, solo se re-renderiza el
ruleset (que ya es declarativo y atómico). El body reemplaza la política
entera, no hace merge.

Decisión de diseño clave: el chain `forward` matchea **sin estado** — no tiene
el `ct state established,related accept` que sí tiene `input`. Cada paquete de
un flujo iniciado por un guest re-evalúa la política en cada pasada, así que
al endurecer las reglas los flujos abiertos bajo la política anterior mueren
en el siguiente paquete: una entrada conntrack obsoleta no puede colarse por
un accept de established, y no hace falta el binario `conntrack(8)` para
flushear nada. Las respuestas (tráfico de vuelta desde la WAN) no las toca
ningún drop (todos van scoped por `iifname` de bridge), pasan por policy
accept.

## Egress y coexistencia con el firewall del host (LEER — origen de bugs sutiles)

La salida a internet de una red `egress: true` depende de **dos condiciones a
nivel de host** que MicroHosted resuelve automáticamente. Documentadas aquí a
fondo porque son la causa nº1 de "la red funciona pero no hay internet", y el
comportamiento cambia según haya Docker en la máquina o no.

### Cómo funciona el egress

Para que un guest (p.ej. `172.16.2.2`) alcance `8.8.8.8`:

1. El guest manda el paquete a su gateway (la IP del bridge, `172.16.2.1`) a
   nivel L2.
2. El host lo **enruta** (destino no es local) → pasa por el hook `forward` de
   netfilter.
3. El host lo **enmascara** (`MASQUERADE`) por su interfaz de salida (hook
   `postrouting`, NAT), reescribiendo el origen a la IP del host.
4. La respuesta vuelve, conntrack la reconoce (`established`) y la desenmascara.

Nada de esto usa el hook `input` (que es solo para tráfico destinado al propio
host) — por eso el guest puede **enrutar a través** del gateway aunque tenga
prohibido **hablar con** el gateway (`ping 172.16.2.1` sigue bloqueado). Esto es
correcto y deseado.

### Condición 1 — IP forwarding del kernel

Con `net.ipv4.ip_forward = 0` el kernel descarta el paquete en el paso 2, antes
de que el NAT actúe. MicroHosted lo activa solo (`network.EnsureIPForward`,
`internal/network/forward.go`) en cuanto existe **alguna** red con egress. Nunca
lo desactiva (el host puede tenerlo activo por otros motivos).

### Condición 2 — el firewall del host no debe descartar el FORWARD

**Semántica clave de netfilter**: en un mismo hook (p.ej. `forward`) pueden
coexistir **varias cadenas base** de tablas distintas, y **todas se evalúan**.
Un veredicto `drop` en *cualquiera* de ellas es **final** y descarta el paquete;
un `accept` solo termina *esa* cadena, no impide que otra cadena posterior lo
dropee. Consecuencia: **que nuestra tabla `inet microhosted` haga `accept` NO
basta** si otra tabla del host dropea el forward.

#### Sin Docker (caso habitual)

La política por defecto del hook `forward` suele ser `accept`. Nuestra tabla
aplica su política fina (drop guest→host, drop cross-segment, drop no-egress→WAN;
accept + masquerade para egress) y **el egress funciona directo**. MicroHosted no
toca ningún firewall del host. Nada que configurar.

#### Con Docker (rompe el egress hasta coordinarse)

Docker instala en la tabla `ip filter` (gestionada por iptables-nft) una cadena
`FORWARD` con **`policy drop`**, y solo hace `accept` del tráfico de *sus*
bridges. El tráfico de *nuestros* bridges cae en ese `drop` → **sin internet**,
aunque `ip_forward=1` y nuestro `masquerade` estén bien. (El cross-segment sí se
bloquea igual, porque ahí gana *nuestro* `drop` — de ahí el síntoma
característico: "inter-VM y aislamiento OK, pero egress no sale").

Solución (patrón estándar, el mismo que usa libvirt): Docker expone la cadena
`DOCKER-USER`, a la que salta **antes** de su propia lógica de drop, justo para
que herramientas externas permitan su tráfico. MicroHosted
(`network.EnsureDockerForwarding`) añade ahí, de forma idempotente:

```
iptables -t filter -I DOCKER-USER -i mhbr+ -j ACCEPT
iptables -t filter -I DOCKER-USER -o mhbr+ -j ACCEPT
```

`mhbr+` es el comodín de iptables para "cualquier interfaz `mhbr...`", así que
dos reglas estáticas cubren todos los bridges presentes y futuros. Se ejecuta
solo si existe la cadena `DOCKER-USER` (Docker presente) y solo cuando hay una
red con egress. **No debilita el aislamiento**: nuestra tabla `inet microhosted`
se sigue evaluando y sus `drop` (guest→host, cross-segment, no-egress→WAN)
siguen ganando por la regla del `drop`-final. El `accept` en `DOCKER-USER` solo
evita que el `drop` genérico de Docker se adelante a nuestra política.

**Best-effort, nunca fatal**: si programar `DOCKER-USER` o `ip_forward` falla,
se registra un *warning* en el journal y el daemon sigue vivo (no tumba la
gestión de las VMs). La tabla `inet microhosted` sí es autoritativa y sí falla
fuerte. Nota: si **reinicias el servicio Docker**, recrea sus cadenas y puede
tirar nuestras reglas de `DOCKER-USER`; se reponen solas al siguiente
crear/borrar red o reinicio de microhosted.

#### Otros firewalls (ufw / firewalld activos)

Mismo principio: si `ufw` o `firewalld` están **activos** con el forward por
defecto en `drop`, pueden descartar el egress. MicroHosted **no** los reconfigura
automáticamente (solo Docker, por su punto de extensión estándar). Si usas uno
de ellos con egress, permite el forward de los bridges `mhbr+`:

```bash
# ufw: política de forward permisiva (o una regla específica para mhbr+)
sudo sed -i 's/^DEFAULT_FORWARD_POLICY=.*/DEFAULT_FORWARD_POLICY="ACCEPT"/' /etc/default/ufw && sudo ufw reload
# firewalld: poner los bridges en una zona que permita forward, p.ej. trusted
sudo firewall-cmd --permanent --zone=trusted --add-interface=mhbr+ ; sudo firewall-cmd --reload
```

### Matriz de comportamiento (egress: true)

| Host | Política `forward` ajena | Qué hace MicroHosted | Resultado |
|------|--------------------------|----------------------|-----------|
| Sin firewall extra | `accept` | nada (nuestra tabla + `ip_forward`) | egress OK |
| Docker | `drop` (cadena de Docker) | `ip_forward` + `DOCKER-USER` accept auto | egress OK |
| ufw/firewalld activos con forward `drop` | `drop` | `ip_forward` (no toca ufw/firewalld) | egress **falla** → aplica el fix de arriba |

### Diagnóstico rápido si egress no sale

```bash
cat /proc/sys/net/ipv4/ip_forward                 # debe ser 1
sudo nft list table inet microhosted              # ¿está la línea 'masquerade' de tu subred?
sudo nft list ruleset | grep -iB1 -A4 'hook forward'  # ¿HAY otra cadena forward con 'policy drop'? (Docker/ufw/firewalld)
sudo iptables -t filter -S DOCKER-USER 2>/dev/null    # con Docker: deben estar las 2 reglas 'mhbr+ ACCEPT'
# desde el guest: ruta por defecto + ARP del gateway
curl -s -X POST localhost:8080/v1/vms/<id>/exec -d '{"cmd":"ip route; ip neigh"}'
```

Regla mental: **egress roto casi siempre = otra cadena `forward` con `policy
drop` (Docker/ufw/firewalld) o `ip_forward=0`**. El aislamiento (guest→host,
cross-segment) NO depende de nada de esto — lo impone nuestra tabla y siempre
gana.

## DNS en el guest (LEER — síntoma: `ping <IP>` funciona, `ping <dominio>` no)

Síntoma característico: `ping 8.8.8.8` responde bien, pero `ping google.com` da
`Temporary failure in name resolution`. **No es un problema de red ni de
firewall** (el tráfico IP ya funciona, egress ya está validado) — es que al
guest nunca le llega qué servidor DNS usar, o le llega pero el guest no lo
aplica. Dos piezas, las dos ya resueltas en el código/imagen base de este
repo, documentadas para que cualquier imagen nueva las tenga en cuenta.

### Pieza 1 — decirle al guest qué DNS usar (lado MicroHosted, ya hecho)

`firecracker-go-sdk`'s `IPConfiguration` acepta hasta 2 `Nameservers`, pero
**nunca se los pasábamos** — solo IP/gateway. `internal/firecracker/machine.go`
ahora fija `defaultNameservers = ["1.1.1.1", "8.8.8.8"]` para toda VM con red.

**Por qué DNS públicos y no los del host**: el resolver del host (p.ej. el stub
`127.0.0.53` de systemd-resolved) sería inalcanzable de todas formas —
`guest→host` está bloqueado por diseño (ver la sección de aislamiento arriba).
Usar 1.1.1.1/8.8.8.8 es coherente con el modelo: en una red `egress:false` esas
IPs son tan inalcanzables como cualquier otra IP externa (el DNS falla igual
que fallaría cualquier tráfico WAN — correcto, malware-safe); en una red
`egress:true` son alcanzables vía el mismo NAT que ya sale a internet, así que
DNS funciona sin ninguna pieza adicional.

### Pieza 2 — que el guest APLIQUE esos nameservers (lado imagen, en `prepare-image.sh`)

Aquí está la parte no obvia. El SDK **no** edita `/etc/resolv.conf` del guest
directamente (no tiene forma de tocar el filesystem del rootfs desde fuera).
Lo que hace es meter IP+gateway+nameservers como parámetro de arranque del
kernel (`ip=`), un mecanismo heredado de netboot/nfsroot. El **kernel Linux**,
al arrancar con ese parámetro, escribe esa configuración en
`/proc/net/pnp` — un pseudo-archivo con sintaxis compatible con
`/etc/resolv.conf` (líneas `nameserver X.X.X.X`).

Pero que `/proc/net/pnp` tenga los DNS no sirve de nada si **nada en el guest
lee de ahí**. La mayoría de distros modernas (systemd-resolved, netplan,
cloud-init, NetworkManager) gestionan `/etc/resolv.conf` a su manera —
normalmente symlink a su propio stub — e ignoran `/proc/net/pnp` por completo.
Sin este paso, el guest simplemente no tiene ningún nameserver configurado,
pase lo que pase del lado de MicroHosted.

**Arreglo**: `scripts/prepare-image.sh` ahora, en cada preparación de imagen
(incondicional, no depende de `--no-ssh`/`--no-vsock` — es red básica, no parte
del acceso), hace:

```bash
# si /etc/resolv.conf era un archivo real, se guarda como .microhosted-orig
ln -sf /proc/net/pnp /etc/resolv.conf
```

Con eso, cualquier programa del guest que lea `/etc/resolv.conf` (la vía
estándar en Linux) obtiene automáticamente los nameservers que MicroHosted le
pasó al arrancar — sin agente propio, sin systemd-resolved, sin DHCP.

### Por qué esto no depende del host (a diferencia del problema de Docker)

A diferencia de la sección de egress (que depende de qué firewall corre en
**el host**), esto depende únicamente de **la imagen del guest** — es el mismo
arreglo en cualquier host, Docker o no, ufw o no. Por eso vive en
`prepare-image.sh` (se aplica una vez por rootfs dorado) y no en el daemon.

### Si preparas una imagen nueva desde cero (no la que ya trae este repo)

Corre siempre `scripts/prepare-image.sh` sobre el rootfs antes de usarlo como
plantilla — deja lista tanto la resolución DNS como el acceso (vsock/SSH). Si
por lo que sea gestionas el rootfs a mano sin este script, el único requisito
mínimo para DNS es esa línea `ln -sf /proc/net/pnp /etc/resolv.conf` dentro del
rootfs montado.

### Diagnóstico rápido si DNS no resuelve pero las IPs sí

```bash
# ¿el kernel recibió y aplicó los nameservers?
curl -s -X POST localhost:8080/v1/vms/<id>/exec -d '{"cmd":"cat /proc/net/pnp"}'
# ¿resolv.conf apunta ahí?
curl -s -X POST localhost:8080/v1/vms/<id>/exec -d '{"cmd":"ls -la /etc/resolv.conf; cat /etc/resolv.conf"}'
# si /proc/net/pnp tiene los nameservers pero resolv.conf no es el symlink:
# la imagen no pasó por prepare-image.sh (o algo lo sobreescribió al arrancar,
# p.ej. systemd-resolved) — vuelve a correr scripts/prepare-image.sh sobre el
# rootfs dorado y crea una VM NUEVA (las ya clonadas no cambian retroactivamente).
```

**Nota importante para VMs ya creadas**: igual que con cualquier cambio de
`prepare-image.sh`, solo afecta a rootfs clonados **después** de volver a
correrlo sobre la plantilla dorada — `internal/storage.CloneRootfs` copia lo
que hubiera en el momento del clonado. Una VM que ya existía sigue sin DNS
hasta que se destruye y se crea una nueva desde la plantilla actualizada.

## Decisiones

- **Backend**: `nft` (nftables) para nuestra tabla propia `inet microhosted` —
  moderno y aislado del firewall del host. Excepción: la coexistencia con Docker
  usa `iptables` (iptables-nft) para la cadena `DOCKER-USER`, porque es la
  interfaz que Docker gestiona y espera (ver la sección de egress).
- **Red por defecto**: al arrancar se crea `default` si no existe
  (`172.16.0.0/24`, `egress: false`). Una VM sin `network` especificado entra a
  `default`.
- **Subred auto**: si no se indica, se asigna un `/24` secuencial del pool
  `172.16.0.0/12`; el operador puede fijar la subred a mano.
- **Persistencia**: las redes se guardan en SQLite. Al arrancar, el daemon
  recrea bridges + reglas desde el estado persistido (una red sobrevive a un
  reinicio del host, no solo del daemon).

## API

```
POST   /v1/networks     {name, subnet?, egress?}
GET    /v1/networks
GET    /v1/networks/{name}
DELETE /v1/networks/{name}          # falla si tiene VMs conectadas
POST   /v1/vms          {..., network: "<name>"}   # network vacío = default
```

## Estado de implementación

- [x] Modelo de datos (`types.Network`) + persistencia (`store` tabla networks)
- [x] IPAM por-red (`network.Subnet`)
- [x] Primitivas de bridge + TAP enslavado (`network` bridge/tap)
- [x] Reglas nftables por red (`network.ApplyNftables`, declarativo)
- [x] Orquestación (`network.Manager`: crear/borrar red, attach/detach VM) +
      reconcile de bridges al arrancar
- [x] Endpoints `/v1/networks` + `network` en create de VM; `vm.Manager` movido
      del modelo `/30` al de bridges
- [x] Egress: `ip_forward` auto + coexistencia con Docker (`DOCKER-USER`)
- [x] **Validada en hardware (2026-07-02)**: inter-VM misma red OK (0.5ms),
      aislamiento entre redes OK, guest↛host OK, egress `false` bloqueado /
      egress `true` con NAT alcanza `8.8.8.8` sobre un host con Docker, todo con
      el aislamiento intacto.

> **Fase 1 CERRADA.** La cadena de bugs que destapó la validación en hardware
> (todos restos de suposiciones del viejo modelo `/30` de "una VM = un enlace
> aislado", que el bridge compartido rompe): máscara `/30` hardcodeada →
> broadcast; sin MAC única → colisión en el bridge; `ip_forward=0`; Docker
> dropeando el FORWARD. Todos resueltos y documentados arriba.
