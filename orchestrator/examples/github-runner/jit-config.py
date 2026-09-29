#!/usr/bin/env python3
"""Mints a GitHub Actions runner's just-in-time config.

mh-orchestrator runs this on its host each time it creates a runner VM
(microse.yml: secrets: command:) and writes what it prints into that VM: a
single-use registration for one ephemeral runner, named after the VM. The
token that mints it never leaves this host.

Usage: jit-config.py SCOPE [LABELS]

  SCOPE   OWNER/REPO for a repository's runners, or ORG for an organization's
  LABELS  comma-separated custom labels (default: microhosted), added to
          self-hosted, Linux and the architecture (X64, ARM64): a JIT runner
          gets exactly the labels asked for, none by default

The token: GITHUB_TOKEN in the .env next to this script, which must then be
readable only by its owner (chmod 600 .env); or $GITHUB_TOKEN. It is read
here, never passed on a command line or through mh-orchestrator, so it shows
up in no process list, log or error. A fine-grained token with:

  repository    "Administration: Read and write" on that repository
  organization  "Self-hosted runners: Read and write" on the organization
"""
import json
import os
import platform
import re
import secrets
import sys
import urllib.error
import urllib.request

API = "https://api.github.com"


def env_file():
    """KEY=VALUE lines of the .env next to this script, as mh-orchestrator
    reads them: # comments, an optional "export ", optional quotes."""
    p = os.path.join(os.path.dirname(os.path.abspath(__file__)), ".env")
    vars = {}
    try:
        with open(p) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#"):
                    continue
                k, sep, v = line.removeprefix("export ").partition("=")
                v = v.strip()
                if len(v) >= 2 and v[0] in "'\"" and v[-1] == v[0]:
                    v = v[1:-1]
                if sep:
                    vars[k.strip()] = v
    except FileNotFoundError:
        pass
    return p, vars


def token():
    t = os.environ.get("GITHUB_TOKEN", "").strip()
    if t:
        return t
    p, vars = env_file()
    t = vars.get("GITHUB_TOKEN", "")
    if not t:
        sys.exit(f"no token: put GITHUB_TOKEN=github_pat_... in {p} (then chmod 600 {p})")
    if os.stat(p).st_mode & 0o077:
        sys.exit(f"{p} holds the token and others can read it: chmod 600 {p}")
    return t


def main():
    if len(sys.argv) not in (2, 3) or sys.argv[1] in ("-h", "--help"):
        sys.exit(__doc__)
    scope = sys.argv[1]
    if not re.fullmatch(r"[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)?", scope):
        sys.exit(f"SCOPE {scope!r}: OWNER/REPO or ORG")
    custom = [l for l in (sys.argv[2] if len(sys.argv) == 3 else "microhosted").split(",") if l]
    arch = {"x86_64": "X64", "aarch64": "ARM64", "arm64": "ARM64"}.get(platform.machine(), platform.machine())
    labels = list(dict.fromkeys(["self-hosted", "Linux", arch] + custom))
    path = f"repos/{scope}" if "/" in scope else f"orgs/{scope}"
    # The VM's name, and a suffix: a VM destroyed before it took a job leaves
    # its runner registered, and a later VM may get the same name.
    name = f"{os.environ.get('MH_VM_NAME', 'microhosted')}-{secrets.token_hex(3)}"
    body = json.dumps({"name": name, "runner_group_id": 1, "labels": labels, "work_folder": "_work"}).encode()
    req = urllib.request.Request(
        f"{API}/{path}/actions/runners/generate-jitconfig", data=body, method="POST",
        headers={
            "Authorization": f"Bearer {token()}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
            "Content-Type": "application/json",
        })
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            sys.stdout.write(json.load(r)["encoded_jit_config"])
    except urllib.error.HTTPError as e:
        # On stderr, which the orchestrator reports: GitHub's message, never
        # the token.
        sys.exit(f"GitHub answered {e.code}: {e.read().decode(errors='replace')[:300]}")
    except urllib.error.URLError as e:
        sys.exit(f"reaching {API}: {e.reason}")


if __name__ == "__main__":
    main()
