#!/usr/bin/env python3
"""simulator.py — simulated Modbus TCP sensor, SIDE B (outside the microVM).

The bare minimum: a PASSIVE Modbus TCP slave that only responds when the master
(parser.c inside the VM) queries it. It never initiates anything, like a real
PLC/sensor. It runs on ANOTHER machine on the network (or on the host, on a
different port).

Python stdlib only — no pymodbus, no pip. It implements just enough of the
protocol: FC 0x03 (read holding registers). The values vary over time so that two
consecutive reads differ (showing that the pull reads live state).

    python3 simulator.py              # listens on 0.0.0.0:5020
    python3 simulator.py 0.0.0.0 502  # standard Modbus port (needs root)

Register map (the same contract as parser.c):
  0: temperature (int16 x10 -> C)   2: pressure (uint16 hPa)
  1: humidity    (uint16 x10 -> %)  3: reading counter (uint16)
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
    """The sensor's 'live' state: slow sine + noise, incremental counter."""
    global _counter
    t = time.time() - _start
    temp_c = 22.5 + 2.5 * math.sin(t / 30.0) + random.uniform(-0.3, 0.3)
    humidity = 50.0 + 10.0 * math.sin(t / 45.0) + random.uniform(-1.0, 1.0)
    pressure = 1013 + int(5 * math.sin(t / 60.0)) + random.randint(-1, 1)
    _counter = (_counter + 1) & 0xFFFF
    return [
        int(round(temp_c * 10)) & 0xFFFF,   # 0: temp x10 (int16 two's complement)
        int(round(humidity * 10)) & 0xFFFF,  # 1: humidity x10
        pressure & 0xFFFF,                    # 2: pressure
        _counter,                             # 3: counter
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
    print(f"[simulator] Modbus TCP slave on {host}:{port} — Ctrl-C to stop")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\n[simulator] stopped.")
        server.shutdown()


if __name__ == "__main__":
    main()
