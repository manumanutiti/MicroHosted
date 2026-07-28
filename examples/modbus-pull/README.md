# Modbus TCP pull demo (the bare minimum)

The first end-to-end use case of the pull pattern in
[`docs/iot-edge.md`](../../docs/iot-edge.md): the microVM boots → interrogates its
Modbus sensor → writes a JSON → the host collects it over vsock. Two files, no
dependencies:

- **`simulator.py`** — SIDE B, outside the VM. A passive Modbus TCP slave (only
  responds). Pure Python stdlib, no pymodbus/pip. Simulates the sensor/PLC.
- **`parser.c`** — SIDE A, inside the VM. A Modbus TCP master: reads registers,
  prints JSON to stdout. libc only → a tiny static binary, no python in the
  guest. Any failure (connection, timeout, Modbus exception, short frame) comes
  out as an error JSON with `error_kind` and a non-zero exit — it never takes
  down the VM.

Register map (holding, the contract between the two):

| reg | field | encoding |
|----|-------|--------------|
| 0 | temperature | signed int16, ×10 → °C |
| 1 | humidity | uint16, ×10 → % |
| 2 | pressure | uint16, hPa directly |
| 3 | reading counter | uint16 (increments on each refresh) |

## 1. Start the simulator (on another machine on the network, or on the host)

```sh
python3 simulator.py 0.0.0.0 5020
```

## 2. Compile the static parser (for the Alpine/musl guest)

Inside Alpine, or with the musl toolchain:

```sh
apk add gcc musl-dev              # inside Alpine
gcc -static -O2 -o parser parser.c && strip parser   # ~20-40 KB
```

## 3. The pull, via the microhosted API

With an `alpine-py` VM (or any Alpine) already created as `$VM`, and the
simulator listening on `$SENSOR_IP:5020`:

```sh
# put the binary into the VM (host→guest vsock channel)
curl -s -X PUT "localhost:8080/v1/vms/$VM/files?path=/parser" --data-binary @parser

# the VM interrogates the sensor and returns the JSON over vsock (the guest starts nothing)
curl -s -X POST "localhost:8080/v1/vms/$VM/exec" \
  -H 'content-type: application/json' \
  -d "{\"cmd\":\"chmod +x /parser && /parser $SENSOR_IP 5020\"}"
```

Response (`.output` of the exec):

```json
{"status":"ok","sensor":"192.168.1.50:5020","registers":[223,512,1013,1],
 "values":{"temperature_c":22.3,"humidity_pct":51.2,"pressure_hpa":1013,"reading_counter":1}}
```

That's the minimal cycle: VM boots → connects → reads → emits JSON. For the
transactional mode (restore → 1 read → destroy) and the fine-grained egress that
restricts the VM to `sensor_IP:502`, see IoT-2/IoT-3 in `docs/iot-edge.md`.

## Test without a VM (everything on the host)

```sh
gcc -O2 -o parser parser.c
python3 simulator.py 127.0.0.1 5020 &
./parser 127.0.0.1 5020
```
