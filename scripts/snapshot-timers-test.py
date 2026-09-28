#!/usr/bin/env python3
"""Do guest timers survive a snapshot load on this host?

Boots a bare Firecracker (no jailer, no daemon) from the base-alpine image,
snapshots it, and loads the snapshot three ways: resume_vm=true, resume_vm=false
then resume, and resume_vm=false + a drive rate-limiter PATCH then resume (the
engine's path). In each, `sleep 0.1` must return inside the guest, and the
guest's arch_timer / timer interrupt count must keep growing.

Found 2026-09-28 on an ARM64 host with a GICv2 interrupt controller: every load
path fails — the timer interrupt count freezes, sleeps never return, the vCPU
spins. See docs/engine.md § 7.

Run as a user with access to /dev/kvm (the kvm group), not as root:
  sg kvm -c "python3 -u scripts/snapshot-timers-test.py"
Environment: FC_BIN, FC_KERNEL, FC_ROOTFS (defaults: the engine's store).
Scratch files go to $FC_EXP_DIR (default ~/fc-exp).
"""
import glob, http.client, json, os, shutil, socket, subprocess, sys, time

STORE = "/var/lib/microhosted/store"
FC = os.environ.get("FC_BIN", "/usr/local/bin/firecracker")
KERNEL = os.environ.get("FC_KERNEL") or sorted(glob.glob(f"{STORE}/kernels/vmlinux-*"))[-1]
ROOTFS = os.environ.get("FC_ROOTFS", f"{STORE}/rootfs/base-alpine.ext4")
BASE = os.environ.get("FC_EXP_DIR", os.path.expanduser("~/fc-exp"))
RL = {"bandwidth": {"size": 104857600, "one_time_burst": 134217728, "refill_time": 1000},
      "ops": {"size": 4000, "one_time_burst": 8192, "refill_time": 1000}}


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=30)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(self.path)


def api(d, method, path, body=None):
    c = UnixHTTP(os.path.join(d, "api.sock"))
    c.request(method, path, json.dumps(body) if body is not None else None, {"Content-Type": "application/json"})
    r = c.getresponse()
    data = r.read().decode()
    if r.status >= 300:
        raise RuntimeError(f"{method} {path}: {r.status} {data}")
    return data


def start(d):
    os.makedirs(d, exist_ok=True)
    for f in ("api.sock", "v.sock"):
        try:
            os.unlink(os.path.join(d, f))
        except FileNotFoundError:
            pass
    log = open(os.path.join(d, "fc.log"), "w")
    p = subprocess.Popen([FC, "--api-sock", "api.sock", "--id", "exp" + str(abs(hash(d)) % 100000)], cwd=d, stdout=log, stderr=log)
    for _ in range(100):
        if os.path.exists(os.path.join(d, "api.sock")):
            return p
        time.sleep(0.02)
    raise RuntimeError("api socket never appeared")


def exec_(d, cmd, timeout=5):
    """The guest agent's protocol: CONNECT 52, one command line, output until
    the exit marker."""
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    try:
        s.connect(os.path.join(d, "v.sock"))
        s.sendall(b"CONNECT 52\n")
        ack = b""
        while not ack.endswith(b"\n"):
            b = s.recv(1)
            if not b:
                return "<closed before ack: agent not listening>"
            ack += b
        s.sendall(cmd.encode() + b"\n")
        out = b""
        while b"___MICROHOSTED_EXIT___" not in out:
            chunk = s.recv(4096)
            if not chunk:
                break
            out += chunk
        return out.decode(errors="replace").split("___MICROHOSTED_EXIT___")[0].strip()
    except OSError as e:
        return f"<{type(e).__name__}: {e}>"
    finally:
        s.close()


def wait_agent(d, secs=20):
    end = time.time() + secs
    while time.time() < end:
        if exec_(d, "echo up", 1) == "up":
            return True
        time.sleep(0.1)
    return False


def timers(d):
    return exec_(d, "timeout 3 sleep 0.1 && echo SLEEP_OK", 5)


def main():
    src = os.path.join(BASE, "src")
    shutil.rmtree(src, ignore_errors=True)
    os.makedirs(src)
    shutil.copy(ROOTFS, os.path.join(src, "rootfs.ext4"))
    p = start(src)
    api(src, "PUT", "/machine-config", {"vcpu_count": 1, "mem_size_mib": 128})
    api(src, "PUT", "/boot-source", {"kernel_image_path": KERNEL, "boot_args": "quiet console=ttyS0 reboot=k panic=1 pci=off"})
    api(src, "PUT", "/drives/rootfs", {"drive_id": "rootfs", "path_on_host": "rootfs.ext4", "is_root_device": True, "is_read_only": False, "rate_limiter": RL})
    api(src, "PUT", "/vsock", {"guest_cid": 3, "uds_path": "v.sock"})
    api(src, "PUT", "/actions", {"action_type": "InstanceStart"})
    print("boot, agent up:", wait_agent(src))
    print("source, fresh      :", timers(src))
    api(src, "PATCH", "/vm", {"state": "Paused"})
    api(src, "PUT", "/snapshot/create", {"snapshot_type": "Full", "snapshot_path": "state", "mem_file_path": "mem"})
    api(src, "PATCH", "/vm", {"state": "Resumed"})
    print("source, after snap :", timers(src))
    print("   source interrupts:", exec_(src, "grep -iE 'timer|LOC:|virtio' /proc/interrupts | head -4", 3).replace("\n", " | "))
    p.kill(); p.wait()

    def restore(name, how):
        d = os.path.join(BASE, name)
        shutil.rmtree(d, ignore_errors=True)
        os.makedirs(d)
        for f in ("state", "mem", "rootfs.ext4"):
            shutil.copy(os.path.join(src, f), os.path.join(d, f))
        q = start(d)
        try:
            how(d)
            wait_agent(d, 10)
            irq = lambda: exec_(d, "grep -iE 'timer|LOC:|virtio' /proc/interrupts | head -4", 3)
            print(f"{name:19}:", timers(d))
            print("   interrupts now  :", irq().replace("\n", " | "))
            time.sleep(2)
            print("   2 s later       :", irq().replace("\n", " | "))
        except Exception as e:
            print(f"{name:19}: ERROR {e}")
        finally:
            q.kill(); q.wait()

    load = lambda d, resume: api(d, "PUT", "/snapshot/load", {"snapshot_path": "state", "mem_backend": {"backend_type": "File", "backend_path": "mem"}, "resume_vm": resume})
    scenarios = {
        "load+resume_vm    ": lambda d: load(d, True),
        "load, then resume ": lambda d: (load(d, False), api(d, "PATCH", "/vm", {"state": "Resumed"})),
        "load, patch, resume": lambda d: (load(d, False), api(d, "PATCH", "/drives/rootfs", {"drive_id": "rootfs", "rate_limiter": RL}), api(d, "PATCH", "/vm", {"state": "Resumed"})),
    }
    only = sys.argv[1:]
    for name, how in scenarios.items():
        if not only or any(o in name for o in only):
            restore(name.strip().replace(" ", "_").replace(",", ""), how)


main()
