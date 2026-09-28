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
  examples/              plant specs that run on stock Alpine images
    first.yaml             the three modes with shell one-liners
    website.yaml           one VM serving a page (a random word per refresh) to
                           the plant LAN through an ingress rule (website/)
    intranet.yaml          three services on one segment (kv, app, logs),
                           clients, a traffic burst and a segmentation check;
                           services in intranet/*.py, shipped with files:
```

## Usage

```bash
mh-orchestrator validate -f examples/first.yaml   # the file on its own
mh-orchestrator plan     -f examples/first.yaml   # what apply would change
mh-orchestrator apply    -f examples/first.yaml   # converge once
mh-orchestrator run      -f examples/first.yaml   # apply, then keep it (Ctrl-C to stop)
mh-orchestrator status   -f examples/first.yaml   # functions, VMs, health, last failures, orphans
mh-orchestrator failures -f examples/first.yaml whoami  # why whoami's VMs failed
mh-orchestrator down     -f examples/first.yaml   # remove everything it owns
```

`run` writes one JSON line per finished cycle to stdout and logs to stderr.
Failure records (cause, output, console tail — captured before the failed VM
is destroyed; the last 5 per function) and *degraded* flags live in
`-state DIR` (default `~/.local/state/mh-orchestrator`, private).
Only one `run`, `apply` or `down` at a time per host user (a lock file in
`$XDG_RUNTIME_DIR`).

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
must be private; a changed file recreates the VM);
cleanup of cycle VMs on stop and of
leftovers on the next start; name collisions with objects the orchestrator does
not own refused before any change.

Changing the spec of a running `run`: edit the file and `mh-orchestrator apply`
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
