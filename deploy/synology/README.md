# Synology deployment workstream

This fork targets a NAS-hosted control plane with external execution machines.
This directory is **under development**, not a service-ready installer.
Do not run the upstream auto-installer on an existing NAS as a substitute: it
can install prerequisites, publish default ports and mount the Docker socket.

`prepare.py` only writes a new, private deployment bundle. It does not invoke
Docker, SSH, package managers or configure a reverse proxy. No existing bundle
is overwritten. The platform image must be the fork's exact digest, not an
upstream release tag that lacks the fork changes.

```sh
python3 deploy/synology/prepare.py \
  --output /path/to/new-private-bundle \
  --data-root /volume1/docker/loopforge-nas/data \
  --image ghcr.io/example/loopforge-fork@sha256:<actual-64-character-digest>
```

Generated JSON is valid YAML and can be consumed as `compose.yaml` by Compose
v2. Secrets are unique per bundle, written only to a mode-0600 `.env`, and are
not printed. Keep the bundle outside this public source tree. Data directories
must be dedicated application subdirectories; the generator does not create
NAS paths or modify their ownership.

The default binding is loopback, signup is disabled, and HTTPS cookies are
required. A reviewed first-administrator provisioning step and HTTPS reverse
proxy are still necessary before use. MySQL has no published port. No host
Docker socket or share root is mounted. Required pool routing is enabled for
all projects; external runners must be registered before real jobs can run.
Existing LoopForge assistant/control processes still run inside the platform
container; this profile alone is not a complete prohibition on every local
process or a multi-tenant isolation guarantee.

Before a release, the following must all have evidence:

- Cross-platform connector binding with independent Agent identities, rotation
  and revocation; no credentials copied from local Agent configuration.
- Codex, DSH and MiniMax Code task adapters with fixed argument contracts,
  cancellation and synthetic end-to-end tests on macOS, Linux and Windows.
- Durable A2A task submit/status/cancel/results boundary and input authorization.
- Explicit handoff requests, old-side stop evidence, complete workspace capture,
  checked restoration and atomic write-ownership transition. Existing filtered
  LoopForge checkpoints are not the full-workspace handoff format.
- Account provisioning, reviewed schema/catalog initialization, ready checks,
  backups, restore rehearsal and rollback without deleting application data.
- Image build/provenance, Compose validation, clean-install browser checks,
  restart/fault tests and supervised acceptance on the target NAS.

No NAS identifiers, addresses, host directories, credentials or private design
notes belong in this directory.

## Local checks

```sh
python3 -m unittest discover -s deploy/synology -v
go test ./internal/connect ./cmd/loopforge-connect
```

The current portable connector has only the `doctor` subcommand. It identifies
Codex (`codex`), DeepSeek Harness (`dsh`) and MiniMax Code (`mcode`) on PATH
using bounded `--version` probes. It does not register, install or launch tasks,
and reports `connected: false`. `mmx` is a different CLI and is not substituted
for MiniMax Code. The future binding/worker protocol must remain separate from
this local preflight.

MiniMax's official headless interface is documented at
https://code.minimax.io/docs/cli/automation. Integration must preserve its
permission policy, session constraints and structured completion status rather
than treating any stdout or zero process exit as a complete task contract.
