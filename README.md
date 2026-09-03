# droidpool

**A pool of disposable Android devices for AI coding agents.**

When several agents work on the same mobile app in parallel, they all end up
installing onto the same test device and overwriting each other. droidpool gives
each agent (or each git worktree) an **exclusive, clean, throwaway** Android
instance backed by [redroid](https://github.com/remote-android/redroid-doc)
containers, plus a browser "device wall" so a human can watch every screen at
once, zoom into one, and take over when an agent needs help.

> Maintained by **Guangzhou Daboshi Supply Chain Co., Ltd.** — <https://daboshi.cn>
> The `fancyCachier` GitHub organization is the company's engineering org.
>
> [中文文档 →](docs/) · License: MIT

## What you get

- **Leases, not devices.** `droidpool claim` in a worktree returns an exclusive
  device; `droidpool release` wipes it and returns it to the pool. Idempotent
  per worktree: calling `claim` twice gives you the same device.
- **A watchdog that assumes agents die.** Three gates reclaim a device: idle
  timeout (no CLI activity for 30 min), TTL (4 h default), and a hard lifetime
  cap (24 h). Heartbeats prove liveness but do not extend the TTL, so a stuck
  agent cannot hold a device forever.
- **Real reset.** Release removes the container **and** wipes its data
  directory via a privileged helper container. Redroid's overlayfs mode
  (`/data-base` + per-device `/data-diff`) makes this a zero-copy operation.
- **Admission control on memory, not swap.** The pool refuses new leases when
  the node's available memory drops below one device's footprint. Swap usage is
  displayed but not used as a gate: it is a lagging, sticky symptom that stays
  high long after pressure is gone.
- **Device wall.** One page shows every device as a live thumbnail with its
  lease, branch and remaining time. Click a tile to zoom: H.264 video via the
  scrcpy protocol decoded with WebCodecs, tap / swipe / keys / text injected
  through scrcpy's control socket, screenshot to PNG or clipboard.
- **Human takeover protocol.** An operator can flag a lease as "human takeover";
  `droidpool status` exits 10 so the agent knows to stop and wait.

## Measured on an 8-core RK3588S with 16 GB RAM

| Metric | Value |
|---|---|
| Container boot to `sys.boot_completed` | 11–13 s |
| Concurrent devices actively driven (p95 within 2× of single) | **10** |
| Resident devices (app idle in foreground) | **12** |
| Video, scrcpy + WebCodecs | 11 fps decoded, 15.8 fps at the server |
| Click-to-pixel latency | **627 ms** median (down from 1 491 ms with `screencap`) |
| Input injection via scrcpy control socket | 42 ms (vs. 119 ms via `adb shell input`) |

The remaining latency is the device's own software rendering; redroid has no
hardware encoder. See `docs/2026-09-03-远程操作方案对比.md` for the full
comparison with scrcpy, ws-scrcpy and STF, and why `screencap`-based streaming
tops out at 3 fps.

## Architecture

```
agent host (Linux / macOS)          control plane (any Linux)         device node (RK3588 / x86)
┌──────────────────────┐            ┌──────────────────────┐           ┌──────────────────────┐
│ droidpool CLI        │── HTTP ───▶│ droidpoold           │─ docker ─▶│ dockerd              │
│  claim / run /       │            │  leases (SQLite)     │  over SSH │  redroid-1 :5561     │
│  release / watch     │            │  watchdog            │           │  redroid-2 :5562     │
│                      │            │  health checker      │           │  …                   │
│ adb -s <ip:port> ────┼────────────┼──────────────────────┼──────────▶│  (adb exposed)       │
└──────────────────────┘            │  device wall (htmx)  │           └──────────────────────┘
                                    │  scrcpy client ──────┼─ adb fwd ──▶ scrcpy-server.jar
operator browser ──── SSE/H.264 ───▶│                      │
                                    └──────────────────────┘
```

The node runs nothing but docker. Everything else lives on the control plane,
so a node can be reimaged in minutes and a second node is one more `[[nodes]]`
block in the config.

## Quick start

### Node

Any Linux host with docker and a kernel that has binderfs and memfd
(RK3588 vendor 6.1 kernels and mainline 6.x both work):

```bash
docker pull redroid/redroid:14.0.0_64only-latest
mkdir -p /data/droidpool
```

### Control plane

```bash
make dist
cp deploy/config.toml.example /opt/droidpool/config.toml   # edit nodes, ports, edge_default
echo "DROIDPOOL_TOKEN=$(openssl rand -hex 16)" > /opt/droidpool/env
deploy/deploy.sh <ssh-alias>     # installs binary, unit file, scrcpy-server jar
```

On first start droidpoold reconciles stale containers on the node, builds a
golden `/data-base` (animations off, screen always on, locale/timezone set), and
brings the pool up to `max_devices`. Optional: set `SCRCPY_SERVER_JAR` to enable
H.264 streaming; without it the wall falls back to `screencap`.

### Agent

```bash
export DROIDPOOL_URL=http://<control-plane>:8600
export DROIDPOOL_TOKEN=<from /opt/droidpool/env>

cd my-worktree
droidpool claim                      # → device id, adb address, lease expiry
droidpool run --apk build/app.apk    # install → seed endpoint → launch → skip onboarding
adb -s $(droidpool addr) shell ...   # drive the UI however you like
droidpool release
```

Long task? `droidpool watch &` keeps the heartbeat alive. See
[`docs/agent-guide.md`](docs/agent-guide.md) (Chinese) for the full playbook,
including the UI-driving pitfalls we hit.

## CLI

| Command | Purpose |
|---|---|
| `claim [--ttl 4h]` | Lease a device for the current worktree (idempotent) |
| `addr` | Print the adb address, for `adb -s $(droidpool addr)` |
| `run [--apk …]` | Install, seed backend endpoint, launch, auto-dismiss first-run dialogs |
| `seed-edge [--host --port]` | Write backend endpoint + certificate pin into the app's prefs |
| `status` | Show lease; exit 10 while a human has taken over |
| `heartbeat` / `watch` | Prove liveness once / continuously |
| `release` | Return the device (it gets wiped and rebuilt) |
| `devices` | List the pool |

## Integrations

| Agent runtime | Integration |
|---|---|
| Claude Code | a skill that wraps the CLI and hooks into the worktree lifecycle |
| DeepSeek harness (dsh) | `/droidpool` command plugin; usage and pitfalls are injected into the system prompt |
| Any MCP client | `droidpool-mcp` (stdio) exposes `droidpool_claim / run / status / heartbeat / release / devices` |

```bash
claude mcp add droidpool -e DROIDPOOL_URL=http://<control-plane>:8600 -e DROIDPOOL_TOKEN=<token> -- droidpool-mcp
```

## HTTP API

Lease endpoints require `Authorization: Bearer <token>`. Wall endpoints
(`/api/wall`, screenshots, streams, input) are unauthenticated by design for
LAN-only deployments; put a reverse proxy in front if you need otherwise.

```
POST   /api/leases                     claim   {host, worktree, branch, head_sha, ttl_min?}
POST   /api/leases/{id}/heartbeat      liveness (does not extend TTL)
POST   /api/leases/{id}/renew          extend TTL
POST   /api/leases/{id}/human          {takeover: bool, note?}
DELETE /api/leases/{id}                release → async reset
GET    /api/devices · /api/leases · /api/health
GET    /api/events                     SSE, full snapshot on every change
GET    /api/devices/{id}/stream.h264   multipart H.264 (scrcpy)
GET    /api/devices/{id}/stream.mjpg   multipart JPEG/PNG (screencap fallback)
POST   /api/devices/{id}/input         {type: tap|swipe|key|text, …}
```

## What it deliberately does not do

- No Bluetooth, USB passthrough, camera or GMS inside the containers. Test those
  on real hardware.
- No hardware video encoding. RK3588 has one, but redroid's Android side only
  ships software codecs; the bottleneck is display readback, not encoding.
- No multi-tenant auth. This is an internal-network tool.

## Repository layout

```
cmd/droidpoold        control plane daemon
cmd/droidpool         agent CLI
cmd/droidpool-mcp     MCP server (stdio) wrapping the HTTP API
internal/pool         lease model, state machine, watchdog, health checker, manager
internal/store        SQLite persistence (idempotent claim, TTL queries)
internal/node         docker-over-SSH node driver, golden image, reconciliation
internal/adb          screenshot / input via adb (fallback path)
internal/scrcpy       scrcpy 4.1 protocol client: video + control socket
internal/api          HTTP API, SSE hub, device wall (embedded htmx pages)
bench/                reproducible smoke, login-flow, concurrency sweep scripts
deploy/               systemd unit, config template, deploy script
docs/                 roadmap, baselines, design comparisons (Chinese)
```

## Development

```bash
make test          # go vet + go test -race
make dist          # cross-compile daemon (linux/amd64) and CLI (linux/amd64, linux/arm64, darwin/arm64)
```

Every package ships with tests that were mutation-checked: we deliberately broke
the code under test and confirmed the relevant test turned red before keeping
it. See `CLAUDE.md` for contributor conventions, including what must never be
committed to this public repository.

## Acknowledgements

- [redroid](https://github.com/remote-android/redroid-doc) — Android in a container
- [scrcpy](https://github.com/Genymobile/scrcpy) — whose server jar and wire
  protocol make low-latency streaming possible; the protocol details here were
  confirmed by packet capture against v4.1
