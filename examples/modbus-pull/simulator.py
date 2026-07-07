#!/usr/bin/env python3
"""simulator.py — sensor Modbus TCP simulado, LADO B (fuera de la microVM).

Lo mínimo indispensable: un slave Modbus TCP PASIVO que solo responde cuando el
maestro (parser.c dentro de la VM) le pregunta. Nunca inicia nada, como un
PLC/sensor real. Corre en OTRA máquina de la red (o en el host, otro puerto).

Solo stdlib de Python — sin pymodbus, sin pip. Implementa lo justo del
protocolo: FC 0x03 (read holding registers). Los valores varían con el tiempo
para que dos lecturas seguidas difieran (se ve que el pull lee estado vivo).

    python3 simulator.py              # escucha en 0.0.0.0:5020
    python3 simulator.py 0.0.0.0 502  # puerto Modbus estándar (necesita root)

Mapa de registros (el mismo contrato que parser.c):
  0: temperatura (int16 x10 -> C)   2: presión (uint16 hPa)
  1: humedad     (uint16 x10 -> %)  3: contador de lecturas (uint16)
"""

import math
import random
import socket
import socketserver
import struct
import sys
import threading
import time

FC_READ_HOLDING = 0x03
_start = time.time()
_counter = 0


def current_registers():
    """Estado 'vivo' del sensor: seno lento + ruido, contador incremental."""
    global _counter
    t = time.time() - _start
    temp_c = 22.5 + 2.5 * math.sin(t / 30.0) + random.uniform(-0.3, 0.3)
    humidity = 50.0 + 10.0 * math.sin(t / 45.0) + random.uniform(-1.0, 1.0)
    pressure = 1013 + int(5 * math.sin(t / 60.0)) + random.randint(-1, 1)
    _counter = (_counter + 1) & 0xFFFF
    return [
        int(round(temp_c * 10)) & 0xFFFF,   # 0: temp x10 (int16 en complemento a 2)
        int(round(humidity * 10)) & 0xFFFF,  # 1: humedad x10
        pressure & 0xFFFF,                    # 2: presión
        _counter,                             # 3: contador
    ]


class ModbusHandler(socketserver.BaseRequestHandler):
    def handle(self):
        sock = self.request
        while True:
            header = self._recv_exact(sock, 7)
            if header is None:
                return
            txid, proto, length, unit = struct.unpack(">H H H B", header)
            pdu = self._recv_exact(sock, length - 1)
            if pdu is None or len(pdu) < 1:
                return
            func = pdu[0]

            if func == FC_READ_HOLDING and len(pdu) >= 5:
                start, count = struct.unpack(">H H", pdu[1:5])
                regs = current_registers()
                if start + count > len(regs) or count < 1:
                    resp = self._exception(unit, func, 0x02)  # ILLEGAL_DATA_ADDRESS
                else:
                    data = struct.pack(">" + "H" * count, *regs[start:start + count])
                    body = struct.pack(">B B", func, count * 2) + data
                    resp = struct.pack(">H H H", txid, 0, len(body) + 1) + \
                        struct.pack(">B", unit) + body
            else:
                resp = self._exception(unit, func, 0x01)  # ILLEGAL_FUNCTION

            try:
                sock.sendall(resp)
            except OSError:
                return

    def _exception(self, unit, func, code):
        body = struct.pack(">B B", func | 0x80, code)
        return struct.pack(">H H H B", 0, 0, len(body) + 1, unit) + body

    @staticmethod
    def _recv_exact(sock, n):
        buf = b""
        while len(buf) < n:
            try:
                chunk = sock.recv(n - len(buf))
            except OSError:
                return None
            if not chunk:
                return None
            buf += chunk
        return buf


class ThreadedServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


def main():
    host = sys.argv[1] if len(sys.argv) > 1 else "0.0.0.0"
    port = int(sys.argv[2]) if len(sys.argv) > 2 else 5020
    server = ThreadedServer((host, port), ModbusHandler)
    print(f"[simulator] slave Modbus TCP en {host}:{port} — Ctrl-C para parar")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\n[simulator] parado.")
        server.shutdown()


if __name__ == "__main__":
    main()
