# Demo pull Modbus TCP (lo mínimo)

Primer caso de uso end-to-end del patrón pull de [`docs/iot-edge.md`](../../docs/iot-edge.md):
la microVM arranca → interroga a su sensor Modbus → escribe un JSON → el host lo
recoge por vsock. Dos ficheros, sin dependencias:

- **`simulator.py`** — LADO B, fuera de la VM. Slave Modbus TCP pasivo (solo
  responde). Python stdlib puro, sin pymodbus/pip. Simula el sensor/PLC.
- **`parser.c`** — LADO A, dentro de la VM. Maestro Modbus TCP: lee registros,
  imprime JSON por stdout. Solo libc → binario estático diminuto, sin python en
  el guest. Cualquier fallo (conexión, timeout, excepción Modbus, trama corta)
  sale como JSON de error con `error_kind` y exit≠0 — nunca tumba la VM.

Mapa de registros (holding, el contrato entre ambos):

| reg | campo | codificación |
|----|-------|--------------|
| 0 | temperatura | int16 con signo, ×10 → °C |
| 1 | humedad | uint16, ×10 → % |
| 2 | presión | uint16, hPa directo |
| 3 | contador de lecturas | uint16 (sube en cada refresco) |

## 1. Arrancar el simulador (en otra máquina de la red, o en el host)

```sh
python3 simulator.py 0.0.0.0 5020
```

## 2. Compilar el parser estático (para el guest Alpine/musl)

Dentro de Alpine, o con la toolchain musl:

```sh
apk add gcc musl-dev              # dentro de Alpine
gcc -static -O2 -o parser parser.c && strip parser   # ~20-40 KB
```

## 3. El pull, vía la API de microhosted

Con una VM `alpine-py` (o cualquier Alpine) ya creada como `$VM`, y el
simulador escuchando en `$SENSOR_IP:5020`:

```sh
# meter el binario en la VM (canal vsock host→guest)
curl -s -X PUT "localhost:8080/v1/vms/$VM/files?path=/parser" --data-binary @parser

# la VM interroga al sensor y devuelve el JSON por vsock (el guest no inicia nada)
curl -s -X POST "localhost:8080/v1/vms/$VM/exec" \
  -H 'content-type: application/json' \
  -d "{\"cmd\":\"chmod +x /parser && /parser $SENSOR_IP 5020\"}"
```

Respuesta (`.output` del exec):

```json
{"status":"ok","sensor":"192.168.1.50:5020","registers":[223,512,1013,1],
 "values":{"temperature_c":22.3,"humidity_pct":51.2,"pressure_hpa":1013,"reading_counter":1}}
```

Eso es el ciclo mínimo: VM arranca → conecta → lee → saca JSON. Para el modo
transaccional (restore → 1 lectura → destroy) y el egress fino que restringe la
VM a `IP_sensor:502`, ver IoT-2/IoT-3 en `docs/iot-edge.md`.

## Prueba sin VM (todo en el host)

```sh
gcc -O2 -o parser parser.c
python3 simulator.py 127.0.0.1 5020 &
./parser 127.0.0.1 5020
```
