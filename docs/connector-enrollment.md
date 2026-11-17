# Portable Agent enrollment (fork development)

The portable connector enrolls one machine plus independent Codex, DSH and
MiniMax Code identities. It is separate from legacy Runner task execution:
**registered identities are not task-ready workers**. Existing Runner dispatch
and isolation are unchanged. The typed queue and transport library are described in
[the delegation protocol](connector-tasks.md); they do not launch native Agents. A2A, native task adapters, full-workspace handoff
and production acceptance remain release blockers.

## User flow

1. Open **Execution machines → Unified Agent identity enrollment** over HTTPS.
2. Download the prebuilt connector for the machine's OS and architecture.
3. In the download directory, run the displayed command, for example:

   ```sh
   chmod +x ./loopforge-connect
   ./loopforge-connect bind --server https://service.example
   ```

   PowerShell uses `./loopforge-connect.exe bind --server https://service.example`.
4. Generate a one-time connector code in the page and paste it at the prompt.
   The code is valid for ten minutes. It is not an Agent API key and is not
   embedded in the command, download URL, local state or command history.
5. The page lists the device and each Agent separately. Revoking an Agent
   leaves its siblings valid; revoking the device invalidates all its Agents.

`--agents all` discovers supported CLIs already on PATH. An explicit selection
such as `--agents codex,minimax-code` fails if a selected CLI is unavailable.
The connector does not install CLIs, change their configuration or copy login
files. Inventory uses only bounded `--version` probes. MiniMax Code uses
`mcode`; the separate `mmx` tool is not substituted.

The current command exits after registration; it does not run a background
worker or claim tasks. Windows binaries are cross-compiled, with native Windows
ACL and runtime acceptance still required before declaring Windows validated.

## Credentials and recovery

- Codes, device credentials and Agent credentials have distinct prefixes and
  independent 256-bit random material. Only SHA-256 hashes reach database rows.
- Code consumption and all identity inserts share one database transaction.
  Failed transactions leave the code usable; concurrent uses have one winner.
- Pending local credentials are committed **before** registration is sent.
  If the response is lost, rerunning with the same state directory verifies the
  saved device credential and recovers the original IDs without reusing a code.
- The default state directory is `~/.loopforge-connect`. A second server needs
  a separate `--state-dir`. Existing credentials are never silently rebound to
  another server or replaced after revocation/expiry.
- Unix state directories/files require private permissions. Windows uses a
  protected DACL for the current user, including existing state files. Advisory
  locks prevent simultaneous modification and are released by process exit.
- Device credentials expire after thirty days; Agent validity also depends on
  the parent device. Automatic rotation and adding further Agents to an existing
  enrollment are not implemented yet. Until then, revoke and explicitly enroll
  into a new dedicated profile; do not delete an active profile as a reset step.
- Local state protects against other OS users, not hostile programs running as
  the same user. Task execution must keep connector credentials out of Agent
  environments and apply a separate execution boundary; enrollment alone does
  not solve child-process filesystem isolation.

The client accepts HTTPS origins only, except literal loopback HTTP for local
synthetic testing. It does not follow redirects, put credentials in query
parameters, print raw server error bodies, or persist the one-time code.
Server responses and management lists exclude hashes and raw credentials.

## API contract

| Route | Authentication | Purpose |
| --- | --- | --- |
| `POST /api/connectors/bind-codes` | User session/API token | Issue a private, ten-minute code |
| `GET /api/connectors` | User session/API token | List only the caller's devices and Agents |
| `POST /api/connectors/devices/{id}/revoke` | Owner | Revoke device and child authentication |
| `POST /api/connectors/agents/{id}/revoke` | Owner | Revoke one Agent |
| `POST /runners/connectors/register` | One-time code in bounded JSON | Atomically enroll device and Agents |
| `GET /runners/connectors/self` | Device bearer credential | Recover the public identity set |
| `GET /runners/connectors/agent` | Agent bearer credential | Verify just that Agent's identity |
| `GET /runners/loopforge-connect?os=…&arch=…` | Public read-only artifact | Download a deployed connector |

Registration only accepts `codex`, `dsh`, `minimax-code`; `linux`, `darwin`,
`windows`; and `amd64`, `arm64`. It carries no arbitrary command, workspace path,
model key or automatic write grant. Malformed/unknown/oversized JSON is rejected.
Machine-plane credentials do not authenticate user-management endpoints.

## Deployment and schema

`scripts/build-binaries.sh --helpers` also builds the portable connector for all
six OS/architecture pairs. Windows files have `.exe`; legacy Runner support
remains Linux/macOS. The NAS only serves prebuilt files, never compiles on a
browser download. Configure:

```text
AGENTFLOW_CONNECTOR_BINARY=/app/bin/loopforge-connect-{os}-{arch}{ext}
```

`metadata=1` on the download route returns the artifact SHA-256, using the same
artifact read boundary as other platform binary downloads. This is integrity
metadata from the trusted server, not independent release signing.

Three additive tables are registered with the existing incremental schema
mechanism. `migrations/033_connector_identities.sql` is the corresponding manual
DDL for reviewed upgrades. No account, production credential or catalog data is
seeded. Retention/rate limits for enrollment codes and production MySQL fault
acceptance still need to be completed before release.

## Validation

Run the frontend baseline build before Go tests as CONTRIBUTING requires.
Tests use temporary SQLite databases, synthetic CLIs and loopback HTTP servers;
no actual model or remote machine is needed.

```sh
python3 -m unittest discover -s deploy/synology -v
go test ./internal/repo -run '^TestConnector' -count=1
go test ./internal/connect ./cmd/loopforge-connect
go test ./internal/httpapi -run '^TestConnector' -count=1
node --test webtest/tests/portable-connectors.test.mjs
```

These checks do not replace browser acceptance, container installation,
production database concurrency tests or native Windows acceptance.
