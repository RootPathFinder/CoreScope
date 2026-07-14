# Deploy CoreScope

Pre-built images are published to GHCR for `linux/amd64` and `linux/arm64` (Raspberry Pi 4/5).

## Using this repository from your own fork

The CI workflow publishes images to the GHCR package that matches the repository slug:

- Repository: `github.com/<owner>/<repo>`
- Image: `ghcr.io/<owner>/<repo>` (lower-cased)

Examples:

- `github.com/acme/CoreScope` → `ghcr.io/acme/corescope`
- `github.com/YourOrg/mesh-analyzer` → `ghcr.io/yourorg/mesh-analyzer`

If the package is private, configure Portainer to use a GitHub Container Registry credential with `read:packages` scope.

### CI notes for forks

- `:edge` is published after the **Go Build & Test** job (not after Playwright). Playwright still runs in parallel on PRs.
- **Merge commits** on `master` skip go-test + Playwright (PR already validated) and publish `:edge` only (~5 min vs ~25 min).
- **Deploy Staging** only runs when the repository variable `STAGING_ENABLED` is set to `true` *and* a self-hosted runner labeled `meshcore-runner-2` is available. Leave it unset for Portainer-only forks so CI is not blocked waiting for a runner that does not exist.
- **Squad Heartbeat** runs every 4 hours (not every 30 min) to reduce Actions noise when Ralph triage is idle.

## Managed repeaters (admin password vault + companion poller)

CoreScope can store admin credentials for remote MeshCore repeaters and poll them over RF via a local **USB Serial Companion**.

### Milestone 1 — vault

- Passwords encrypted at rest under `/app/data/managed-repeaters.json` (never returned by the API)
- Set a strong `apiKey` in `config.json` (required for vault API access)
- Optional: `CORESCOPE_VAULT_KEY` (preferred). If unset, the vault key is derived from `apiKey`
- UI: `#/repeaters`

### Milestone 2 — companion poller

Binary: `/app/corescope-companion-poller` (same image).

It opens the companion serial port, lists contacts (`CMD_GET_CONTACTS`), logs into each vaulted repeater (`CMD_SEND_LOGIN`), requests status (`CMD_SEND_STATUS_REQ`), and writes `/app/data/managed-repeater-status.json`. The CoreScope server merges that into `GET /api/managed-repeaters` (`poll.stats`, `companionKnown`, and `contacts[]`).

Poller logs are verbose on failure: contact count, whether the pubkey is among companion contacts, and a hint to flood an advert toward the USB companion (MQTT knowing a node is not enough).

**Requirements:**

- Companion flashed as **Serial Companion** firmware (not repeater)
- Device mounted into the poller container (e.g. `/dev/ttyACM1`)
- Prefer a **powered USB hub / solid 5V supply** — LoRa TX (especially flood) can brown out weak USB ports and show as `EOF` / “companion USB disconnected during RF TX”. The poller forces **zero-hop** paths for seeded contacts (not flood), reconnects on disconnect, and continues to the next repeater. If disconnects persist on zero-hop too: confirm only one process owns the tty, try the same login with official `meshcore-cli`, and check companion firmware stability on RF TX — not only the PSU.
- Admin passwords ≤ **15 characters** (companion protocol limit)
- UI `#/repeaters` shows companion contacts and marks each vaulted repeater as **On companion** / **Not on companion**
- **Test USB** button on the companion card runs an on-demand self-test (open serial → `APP_START` → `GET_CONTACTS`, no RF login). Requires `apiKey`. The poller picks up the request within ~1s and updates status.
**Portainer sidecar example:**

```yaml
  companion-poller:
    image: ghcr.io/<owner>/<repo>:edge
    container_name: corescope-companion-poller
    restart: unless-stopped
    user: root
    devices:
      - "/dev/ttyACM1:/dev/ttyACM1"
    environment:
      CORESCOPE_VAULT_KEY: "${CORESCOPE_VAULT_KEY}"
      COMPANION_SERIAL: "/dev/ttyACM1"
      COMPANION_BAUD: "115200"
      COMPANION_POLL_INTERVAL: "5m"
    volumes:
      - corescope_data:/app/data
    entrypoint:
      - /app/corescope-companion-poller
    command:
      - -config-dir=/app
      - -serial=/dev/ttyACM1
```

Share the same `corescope_data` volume with the main `corescope` service so the poller can read the vault and the UI can read status.

Do **not** point `meshcoretomqtt` at the companion port — keep observer ingest and admin polling on separate devices/roles.


## Quick Start

### Docker run

```bash
docker run -d --name corescope \
  -p 80:80 \
  -v corescope-data:/app/data \
  -e DISABLE_CADDY=true \
  ghcr.io/kpa-clawbot/corescope:latest
```

Open `http://localhost` — done.

### Docker Compose

```bash
curl -sL https://raw.githubusercontent.com/Kpa-clawbot/CoreScope/master/docker-compose.example.yml \
  -o docker-compose.yml
docker compose up -d
```

### Portainer stack (external MQTT broker, no built-in Caddy/Mosquitto)

Use this as a baseline when CoreScope should connect to an existing MQTT broker on a shared Docker network.

```yaml
services:
  corescope:
    image: ghcr.io/<owner>/<repo>:vX.Y.Z
    container_name: corescope
    restart: unless-stopped
    ports:
      - "8080:3000"
    environment:
      - DISABLE_MOSQUITTO=true
      - DISABLE_CADDY=true
    networks:
      - default
      - mqtt_default
    entrypoint:
      - sh
      - -c
      - |
        mkdir -p /app/data
        if [ ! -f /app/data/config.json ]; then
          cp /app/config.example.json /app/data/config.json
        fi
        exec /entrypoint.sh
    volumes:
      - corescope_data:/app/data

networks:
  mqtt_default:
    external: true

volumes:
  corescope_data:
```

Recommended image strategy for Portainer:

- Production: pin to `vX.Y.Z` and update intentionally
- Validation/staging: use `edge`
- Avoid `latest` when reproducibility matters

## Image Tags

| Tag | Description |
|-----|-------------|
| `v3.4.1` | Pinned release (recommended for production) |
| `v3.4` | Latest patch in v3.4.x |
| `v3` | Latest minor+patch in v3.x |
| `latest` | Latest release tag |
| `edge` | Built from master — unstable, for testing |

## Configuration

Settings can be overridden via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `DISABLE_CADDY` | `false` | Skip internal Caddy (set `true` behind a reverse proxy) |
| `DISABLE_MOSQUITTO` | `true` in `docker-compose.staging.yml`; `false` elsewhere | Skip internal MQTT broker. Default flipped to `true` for the staging deploy in v3.7+ because a standalone `mqtt-broker` container owns MQTT on that host — see "Standalone MQTT broker (staging)" below. |
| `HTTP_PORT` | `80` | Host port mapping |
| `DATA_DIR` | `./data` | Host path for persistent data |

For advanced configuration, mount a `config.json` into `/app/data/config.json`. See `config.example.json` in the repo.

## Updating

```bash
docker compose pull
docker compose up -d
```

## Data

All persistent data lives in `/app/data`:
- `meshcore.db` — SQLite database (packets, nodes)
- `config.json` — custom config (optional)
- `theme.json` — custom theme (optional)

**Backup:** `cp data/meshcore.db ~/backup/`

## TLS

Option A — **External reverse proxy** (recommended): Run with `DISABLE_CADDY=true`, put nginx/traefik/Cloudflare in front.

Option B — **Built-in Caddy**: Mount a custom Caddyfile at `/etc/caddy/Caddyfile` and expose ports 80+443.

---

## Standalone MQTT broker (staging)

Starting in v3.7, `docker-compose.staging.yml` assumes a **standalone
`mqtt-broker` container** (image: `eclipse-mosquitto:2`) already runs
on the staging VM, out-of-band from this repo. That container:

- Owns port `8883` externally (TLS-terminated MQTT for real observers).
- Is attached to a shared docker network named `meshcore-net`.
- Is operator-managed state — it is **not** defined in any compose
  file in this repository. Its config, TLS certs, and ACLs live on the
  host, outside git.

`corescope-staging-go` reaches it in-network at `mqtt-broker:1883` via
docker DNS (no host port hop). To make that work, `docker-compose.staging.yml`
joins the external `meshcore-net` network and defaults `DISABLE_MOSQUITTO=true`
so the built-in mosquitto stays off.

### Prereq — one-time provisioning on the staging host

```bash
docker network create meshcore-net
# ...then bring up the operator-managed mqtt-broker container on that
# network (not covered here; that's operator state). THEN:
docker compose -f docker-compose.staging.yml up -d
```

If `meshcore-net` doesn't exist when compose starts, docker will refuse
to bring `staging-go` up (`external: true` — compose won't create it).

### Reverting to the old single-container behaviour

Third-party operators cloning this repo who want the legacy shape
(in-container mosquitto + `1883:1883` on the host, no external broker)
should override both the env default and re-add the port mapping.

In `.env` (or the shell):

```
DISABLE_MOSQUITTO=false
```

And in `docker-compose.staging.yml`, restore the `1883:1883` mapping
under `services.staging-go.ports`:

```yaml
    ports:
      - "${STAGING_GO_HTTP_PORT:-80}:80"
      - "${STAGING_GO_MQTT_PORT:-1883}:1883"   # ← re-added
      - "6060:6060"
      - "6061:6061"
```

That gives you back the pre-v3.7 self-contained staging shape. In that
mode you do **not** need `meshcore-net`, but note the compose file still
declares it as `external: true`, so either remove that declaration in
your fork or ensure the network exists.

---

## Migrating from manage.sh (existing admins)

If you're currently deploying with `manage.sh` (git clone + local build), you have two options going forward:

### Option A: Keep using manage.sh (no changes needed)

`manage.sh update` continues to work exactly as before — it fetches the latest tag, builds locally, and restarts. Nothing breaks.

```bash
./manage.sh update          # latest release
./manage.sh update v3.5.0   # specific version
```

### Option B: Switch to pre-built images (recommended)

Pre-built images skip the build step entirely — faster updates, no Go toolchain needed.

**One-time migration:**

1. Stop the current deployment:
   ```bash
   ./manage.sh stop
   ```

2. Your data is in `~/meshcore-data/` (or whatever `PROD_DATA_DIR` is set to). It's untouched — the database, config, and theme files persist.

3. Copy `docker-compose.example.yml` to where you want to run from:
   ```bash
   cp docker-compose.example.yml ~/docker-compose.yml
   ```

4. Start with the pre-built image:
   ```bash
   cd ~ && docker compose up -d
   ```

5. Verify it picked up your existing data:
   ```bash
   curl http://localhost/api/stats
   ```

**Updates after migration:**
```bash
docker compose pull && docker compose up -d
```

### What about manage.sh features?

| manage.sh command | Pre-built equivalent |
|---|---|
| `./manage.sh update` | `docker compose pull && docker compose up -d` |
| `./manage.sh stop` | `docker compose down` |
| `./manage.sh start` | `docker compose up -d` |
| `./manage.sh logs` | `docker compose logs -f` |
| `./manage.sh status` | `docker compose ps` |
| `./manage.sh setup` | Copy `docker-compose.example.yml`, edit env vars |

`manage.sh` remains available for advanced use cases (building from source, custom patches, development). Pre-built images are recommended for most production deployments.

## Staging VM — disk-usage monitor & cleanup (#1684)

The staging VM ran out of disk during a hot-patch (#1684). To prevent
repeats, two scripts live in `scripts/staging/`:

- `disk-monitor.sh <mount>` — reads `df -P`, classifies usage against
  `<80 ok / >=80 warn / >=90 error / >=95 alert`, emits to stderr +
  journald (via `logger`). Returns non-zero on `error|alert` so
  systemd surfaces the unit as failed.
- `disk-cleanup.sh` — removes `/tmp` snapshot files (`*.db`,
  `staging-snap.*`, `cs-*`, `node-compile-cache`) older than 7 days
  and runs `docker builder prune` + `docker image prune` with
  `--filter "until=72h" --filter "label!=keep"`. Set
  `CORESCOPE_CLEANUP_DRY_RUN=1` to log without deleting.

### Install on the staging host

SSH to `<STAGING_HOST>` as the staging operator user and:

```bash
sudo install -m 0755 scripts/staging/disk-monitor.sh  /usr/local/bin/corescope-disk-monitor
sudo install -m 0755 scripts/staging/disk-cleanup.sh  /usr/local/bin/corescope-disk-cleanup

# 15-minute monitor
sudo tee /etc/systemd/system/corescope-disk-monitor.service >/dev/null <<'UNIT'
[Unit]
Description=CoreScope staging disk-usage monitor (issue #1684)
[Service]
Type=oneshot
ExecStart=/usr/local/bin/corescope-disk-monitor /
UNIT

sudo tee /etc/systemd/system/corescope-disk-monitor.timer >/dev/null <<'UNIT'
[Unit]
Description=Run CoreScope disk-usage monitor every 15 minutes
[Timer]
OnBootSec=5min
OnUnitActiveSec=15min
Unit=corescope-disk-monitor.service
[Install]
WantedBy=timers.target
UNIT

# Daily cleanup at 03:30 local
sudo tee /etc/systemd/system/corescope-disk-cleanup.service >/dev/null <<'UNIT'
[Unit]
Description=CoreScope staging disk cleanup (issue #1684)
[Service]
Type=oneshot
ExecStart=/usr/local/bin/corescope-disk-cleanup
UNIT

sudo tee /etc/systemd/system/corescope-disk-cleanup.timer >/dev/null <<'UNIT'
[Unit]
Description=Run CoreScope disk cleanup daily at off-peak
[Timer]
OnCalendar=*-*-* 03:30:00
Persistent=true
Unit=corescope-disk-cleanup.service
[Install]
WantedBy=timers.target
UNIT

sudo systemctl daemon-reload
sudo systemctl enable --now corescope-disk-monitor.timer corescope-disk-cleanup.timer
```

`<STAGING_HOST>` is the staging VM hostname/IP — operator supplies it,
not committed to the repo.

### Inspecting alerts

```bash
journalctl -t corescope-disk-monitor   --since '-1d'
journalctl -t corescope-disk-cleanup   --since '-7d'
systemctl list-timers | grep corescope-disk
```

`logger` priorities map: `ok→info`, `warn→warning`, `error→err`,
`alert→alert` (syslog severity 1, the highest level). Wire
`journalctl -p alert ...` to whatever ops channel the operator
prefers; use `-p err` to also catch the `error` tier.

### Notes on `staging-snap.db` root cause (#1684 phase 3)

`grep -rn staging-snap.db cmd/ public/ scripts/` returns **zero**
hits in the repo. The 4.4 GB orphan was a manual debugging artifact,
not produced by any committed code. The `disk-cleanup.sh` retention
rule (anything matching `staging-snap.*` in `/tmp` older than 7 days)
prevents recurrence without needing source-side TTL changes.

If a future feature legitimately needs persistent snapshot DBs, put
them under `/var/lib/corescope/snapshots/` with explicit rotation —
not in `/tmp`, which is ephemeral by definition.
