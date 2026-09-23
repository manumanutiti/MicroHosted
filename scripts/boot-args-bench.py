#!/usr/bin/env python3
"""Boot-args benchmark: how much of a guest's boot do the kernel arguments cost?

Boots the template's kernel + rootfs directly under Firecracker (no daemon, no
jailer, no network) with several kernel command lines, interleaved, and times
each boot from the InstanceStart call to the vsock agent answering. Every boot
gets a fresh copy of the rootfs, so no run pays for another's unclean shutdown.

It changes nothing on the host: its files live in a temporary directory that is
removed at the end, and every Firecracker it starts is killed.

Needs /dev/kvm (run as root or as a member of the kvm group).

Usage: sudo scripts/boot-args-bench.py [--runs 10]
         [--kernel /var/lib/microhosted/store/kernels/vmlinux-6.1.102]
         [--rootfs /var/lib/microhosted/store/rootfs/base-alpine.ext4]
         [--firecracker /usr/local/bin/firecracker]
"""

import argparse
import http.client
import json
import os
import platform
import shutil
import socket
import statistics
import subprocess
import tempfile
import time

AGENT_PORT = 52
BASE_ARGS = "console=ttyS0 reboot=k panic=1 pci=off"


def variants():
    v = {
        "current": BASE_ARGS,
        "quiet": BASE_ARGS + " quiet",
        "no-serial (ref)": "reboot=k panic=1 pci=off 8250.nr_uarts=0",
    }
    if platform.machine() == "x86_64":
        # Skip probing the emulated PS/2 controller's AUX port, MUX and PnP,
        # keeping the keyboard: Firecracker's SendCtrlAltDel (graceful stop)
        # goes through it.
        i8042 = " i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
        v["i8042"] = BASE_ARGS + i8042
        v["quiet+i8042"] = BASE_ARGS + " quiet" + i8042
    return v


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost")
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(self.path)


def fc_put(api_sock, path, body):
    conn = UnixHTTPConnection(api_sock)
    conn.request("PUT", path, json.dumps(body), {"Content-Type": "application/json"})
    resp = conn.getresponse()
    data = resp.read()
    conn.close()
    if resp.status >= 300:
        raise RuntimeError(f"PUT {path}: {resp.status} {data!r}")


def agent_exec(vsock_path, cmd, timeout=0.5):
    """Runs cmd through the guest agent; returns its output or None if not up."""
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    try:
        s.connect(vsock_path)
        s.sendall(f"CONNECT {AGENT_PORT}\n".encode())
        f = s.makefile("rb")
        if not f.readline().startswith(b"OK "):
            return None
        s.sendall(cmd.encode() + b"\n")
        out = []
        for line in f:
            if line.startswith(b"___MICROHOSTED_EXIT___:"):
                return b"".join(out).decode(errors="replace")
            out.append(line)
        return None
    except OSError:
        return None
    finally:
        s.close()


def wait_for(path, timeout=5.0):
    deadline = time.monotonic() + timeout
    while not os.path.exists(path):
        if time.monotonic() > deadline:
            raise RuntimeError(f"{path} did not appear")
        time.sleep(0.001)


def boot_once(args, workdir, fc_bin, kernel, rootfs, boot_args):
    shutil.rmtree(workdir, ignore_errors=True)
    os.makedirs(workdir)
    disk = os.path.join(workdir, "rootfs.ext4")
    subprocess.run(["cp", "--reflink=auto", rootfs, disk], check=True)
    api_sock = os.path.join(workdir, "api.sock")
    vsock = os.path.join(workdir, "v.sock")
    log = open(os.path.join(workdir, "console.log"), "wb")
    proc = subprocess.Popen([fc_bin, "--api-sock", api_sock], stdout=log, stderr=log,
                            stdin=subprocess.DEVNULL, cwd=workdir)
    try:
        wait_for(api_sock)
        fc_put(api_sock, "/machine-config", {"vcpu_count": 1, "mem_size_mib": 128})
        fc_put(api_sock, "/boot-source", {"kernel_image_path": kernel, "boot_args": boot_args})
        fc_put(api_sock, "/drives/rootfs", {"drive_id": "rootfs", "path_on_host": disk,
                                            "is_root_device": True, "is_read_only": False})
        fc_put(api_sock, "/vsock", {"guest_cid": 3, "uds_path": vsock})
        t0 = time.monotonic()
        fc_put(api_sock, "/actions", {"action_type": "InstanceStart"})
        t_start = time.monotonic()
        deadline = t0 + 20
        while time.monotonic() < deadline:
            if os.path.exists(vsock) and agent_exec(vsock, "true") is not None:
                break
            time.sleep(0.005)
        else:
            raise RuntimeError(f"agent silent with boot args {boot_args!r}")
        t_ready = time.monotonic()
        uptime = agent_exec(vsock, "cut -d' ' -f1 /proc/uptime") or "-"
        return (t_start - t0) * 1000, (t_ready - t0) * 1000, uptime.strip()
    finally:
        proc.kill()
        proc.wait()
        log.close()


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--runs", type=int, default=10)
    p.add_argument("--kernel", default="/var/lib/microhosted/store/kernels/vmlinux-6.1.102")
    p.add_argument("--rootfs", default="/var/lib/microhosted/store/rootfs/base-alpine.ext4")
    p.add_argument("--firecracker", default="/usr/local/bin/firecracker")
    a = p.parse_args()

    v = variants()
    results = {name: [] for name in v}
    tmp = tempfile.mkdtemp(prefix="boot-args-bench-")
    print(f"==> {platform.machine()}, {a.runs} boots per variant, interleaved")
    try:
        for i in range(a.runs):
            for name, boot_args in v.items():
                start_ms, ready_ms, uptime = boot_once(a, os.path.join(tmp, "vm"), a.firecracker,
                                                       a.kernel, a.rootfs, boot_args)
                results[name].append(ready_ms)
                print(f"  run {i + 1:>2}  {name:<16} InstanceStart {start_ms:5.1f} ms"
                      f"  agent ready {ready_ms:6.1f} ms  guest uptime {uptime}s")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    print("\n==> InstanceStart call → agent answering (median / p95 / min)")
    base = statistics.median(results["current"])
    for name, xs in results.items():
        xs = sorted(xs)
        med = statistics.median(xs)
        p95 = xs[min(len(xs) - 1, int(len(xs) * 0.95))]
        delta = f"{med - base:+.0f} ms" if name != "current" else ""
        print(f"  {name:<16} {med:6.1f} / {p95:6.1f} / {xs[0]:6.1f} ms  {delta}")
        print(f"  {'':<16} {v[name]}")


if __name__ == "__main__":
    main()
