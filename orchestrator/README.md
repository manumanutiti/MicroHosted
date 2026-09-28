# mh-orchestrator

Keeps a MicroHosted host in the state a plant spec declares: the spec's
networks and functions are created, kept in their lifecycle, and whatever the
orchestrator owns that the spec no longer declares is removed. Design and
decisions: [`docs/orchestrator.md`](../docs/orchestrator.md).

It is a client of the engine API over its Unix socket and imports nothing from
the engine's `internal/` packages (a test enforces it). Everything it creates
carries `managed-by=mh-orchestrator`; it never touches an object without that
label.

```
orchestrator/
  cmd/mh-orchestrator/   the binary (make build → build/mh-orchestrator)
  spec/                  strict YAML parsing and full validation
  engine/                engine API client (pkg/types only)
  orch/                  plan, apply, cycles, supervision, status
  examples/              plant specs and the images they build
    alpine-nginx/          build.yml (nginx serving site/, every field
                           commented) + microse.yml that runs it with build: .
    app-ubuntu/            the same on Ubuntu 24.04: a Python service
    images/                build.yml of the images the flat examples use:
                           alpine, alpine-py
    first.yaml             the three modes with shell one-liners
    website.yaml           one VM serving a page (a random word per refresh) to
                           the plant LAN through an ingress rule (website/)
    intranet.yaml          three services on one segment (kv, app, logs),
                           clients, a traffic burst and a segmentation check;
                           services in intranet/*.py, shipped with files:
```

## Usage

`mh` runs it with docker compose's verbs on the `microse.yml` of the current
directory (`-f FILE` for another):

```bash
cd examples/alpine-nginx          # a build.yml and a microse.yml
mh validate                       # ./microse.yml on its own
mh plan                           # builds the image if needed (sudo), then what would change
mh up                             # apply, then keep it running in this terminal (Ctrl-C to stop)
mh up -d                          # the same in the background; log in the project's state dir
mh apply                          # converge once, or hand changes to the running up
mh status                         # functions, VMs, health, last failures, orphans
mh failures web                   # why web's VMs failed
mh down                           # stop the project's up and remove what it created

mh up -d -f examples/first.yaml   # any other file: -f
```

`mh up` is `mh-orchestrator run`; every verb above is also `mh-orchestrator
VERB`, the program `mh` hands them to (`make install-cli` installs both).

**Projects.** Each plant spec is a project, named by its `name:` or else by its
directory, as docker compose names them. Everything the orchestrator creates
carries `project=<name>`, and a spec sees and changes only its own project's
objects — `mh up` in two directories runs both, `mh down` in one leaves the
other alone. VMs are named `<project>-<function>-<n>`; network names are
shared by the whole host, so two projects cannot both declare `web`. Objects
made before projects existed (no `project` label) are adopted, relabelled in
place, by the spec that declares them. Locks, failure records and the
background log are per project.

`mh up` writes one JSON line per finished cycle to stdout and logs to stderr
(`up -d`: both to `run.log` in the project's state directory). Failure records
(cause, output, console tail — captured before the failed VM is destroyed; the
last 5 per function) and *degraded* flags live in `-state DIR/<project>`
(default `~/.local/state/mh-orchestrator`, private). One `up`, `apply` or
`down` at a time per project and host user (a lock file in
`$XDG_RUNTIME_DIR`); `down` stops the project's `up` first.

## Images

A function's `image:` is a pinned reference, `name:version@sha256:<digest>`,
and the digest is mandatory: a replacement boots exactly the bytes that were
validated. The digest comes from the engine's image store, and there are two
ways to get it there:

- **`build:`** — the function points at a `build.yml` (or the directory
  holding one, relative to the spec), and `plan`/`apply`/`run` run
  [`mh build`](../docs/cli.md#building-an-image-mh-build) on it and pin the
  reference it prints. `mh build` versions an image by the fingerprint of its
  inputs, so an unchanged build context builds nothing and a changed one is a
  new image, rolled out like any change. `validate`, `status`, `down` and a
  reload never build (a running orchestrator has no terminal for `sudo`):
  `apply` builds before it hands a spec over.

  ```yaml
  functions:
    site:
      build: ./site        # ./site/build.yml
  ```
- **`image:`** — an image built elsewhere, pinned by hand or through a
  variable:

  ```yaml
  functions:
    site:
      image: ${SITE}       # from the environment, else from .env next to the spec
  ```

  ```bash
  echo "SITE=$(mh build -q ./site)" >> .env && mh apply
  ```

`${NAME}` follows docker compose (`${NAME:-default}`, `${NAME:?message}`); a
bare `$NAME` is left alone for the guest's shell. Keep the variables in `.env`
when a `run` is going on: it reloads the file with its own environment, and
`apply` refuses to hand it values only your shell has. A function that sets no
`command` (or, persistent, no `health`) takes the image's defaults; `plan` says
so. `make prepare-image` and `mh image import` (`mh image ls -q` prints the
references again) still work for images built by hand. A catalog template (`mh run
alpine-py`, no `:`) has no digest and cannot be used here — see
[`docs/engine.md`](../docs/engine.md#template-or-image).

## Lifecycle modes

| `mode` | Each VM | Success |
|---|---|---|
| `transaction` | created every `every`, runs `command` once, destroyed | exit 0 within `timeout` (boot included) |
| `window` | created every `every`, runs `command` for `duration`, destroyed | the command is still running at the end of the window |
| `persistent` | always on; destroyed and replaced when it dies, fails its health check, or reaches `recycle` | `health` passes (or the command stays up) |

## Current scope

Implemented: strict parsing and validation; networks (create, policy updates
in place, pruning); the three modes; image digests checked against the engine's
store; the static budget; health checks with failure thresholds; back-off and
*degraded* for persistent functions; failure records with the console tail;
`files:` and `secrets:` (read at load, relative to the spec; a secret's source
must be private; a changed file recreates the VM); `${VAR}` from the
environment or `.env`; `build:` (images built by fingerprint, only when
their inputs change); the image's default command and health check;
cleanup of cycle VMs on stop and of
leftovers on the next start; name collisions with objects the orchestrator does
not own refused before any change.

Changing the spec of a running `up`: edit the file and `mh apply`
it — the running orchestrator rolls the changes out one function at a time,
verifying each, and puts a function whose new version fails back on its
previous one (*held*; apply again to retry).

Not yet (see the design): held, back-off and *degraded* kept across an
orchestrator restart (a restart converges to the file as it is and gives every
function its attempts again); updates that revert on a plain `apply` without a
running orchestrator (there, a failure halts the apply); the result journal
(results go to stdout); engine events (the supervisor polls every 2 s); `apply`
asking about quarantined VMs of removed functions (they are kept and listed).

Requires an engine with image creation and `GET /v1/vms/{id}/ready` (commit
d8a7054 or later); failure records include the console tail with an engine that
serves `GET /v1/vms/{id}/console` (without it they say it is unavailable).
