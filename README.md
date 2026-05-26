# rclient

Self-hosted reverse-connect remote shell for your own boxes. Agents dial out
to a central server over a single WSS connection, so the boxes themselves
need no inbound ports, public IPs, or VPNs. The server runs in Docker behind
Caddy on a domain you own, and gives you a small web panel with live metrics
and a real PTY shell for each agent.

Built for managing a handful of personal Linux machines (proxy boxes, home
servers, small VPS fleets) without exposing SSH, opening firewall holes, or
relying on third-party services that may be filtered or unreliable.

## Highlights

- **Reverse connect.** Each agent makes a single outbound `wss://` connection
  to your server. Looks like ordinary HTTPS to anyone watching.
- **Real PTY shell.** `htop`, `vim`, `tail -f`, colour output, resize — all
  work. Multiple shell tabs per agent.
- **Live metrics.** CPU, memory, disk, load, uptime, kernel/version banner.
- **Per-agent enrollment.** First-contact registration mints a per-agent
  secret. A leaked shared bearer token still can't impersonate an existing
  box.
- **Single-binary deployment.** `rclient-agent install` copies itself to
  `/usr/local/bin`, writes the systemd unit (or OpenRC init), and starts.
- **Two install systems supported.** systemd (Debian/Ubuntu/Fedora) and
  OpenRC (Alpine). Auto-detected.
- **No third-party services.** TLS via Let's Encrypt with Cloudflare DNS-01,
  served by Caddy. No CDN, no telemetry, no account.

## Architecture

```
┌──────────────────┐                     ┌────────────────┐
│  rclient-agent   │  ── wss outbound ─▶│   rclient       │ ◀── browser ── you
│  (your box)      │                     │   + Caddy TLS  │
└──────────────────┘                     └────────────────┘
        ▲
        └─ persists agent.id & agent.secret in /var/lib/rclient
```

Server stack: Go backend (single static binary, embedded panel UI), Caddy 2
in front for TLS termination. Agent is a single Go binary with no runtime
dependencies. Panel is vanilla HTML/JS/CSS plus xterm.js, no build step.

## TLS — pick one

`setup.sh` will ask which method to use; here's the trade-off:

- **Cloudflare DNS-01** — domain on Cloudflare, you have an API token with
  `Zone:Read + Zone:DNS:Edit` on the zone. Works even when ports 80/443 are
  busy with other services. Best default if you're on Cloudflare.
- **HTTP-01** — simplest, no API tokens. Caddy listens on port 80 to answer
  the ACME challenge. Requires port 80 free on the VPS.
- **Bring your own** — point at a directory containing `fullchain.pem` and
  `privkey.pem`. No ACME runs at all. Useful for wildcard certs from
  elsewhere or self-signed LAN setups.

## Quick start

You need a small VPS with Docker, and a domain pointed at it.

### Server — one command

```sh
curl -fsSL https://raw.githubusercontent.com/b3s3da/rclient/main/setup.sh | sh
```

That's it. The script clones the repo, asks for your domain and TLS
preference, generates random secrets, brings up the stack, and prints
your panel URL + a connect blob for agents.

If you'd rather see the script first:

```sh
git clone https://github.com/b3s3da/rclient.git
cd rclient
./setup.sh
```

Sample output:

```
Public host:port (e.g. r.example.com:13337): r.example.com:13337

How do you want to get a TLS certificate?
  1) Cloudflare DNS-01
  2) HTTP-01 challenge
  3) Bring your own certificate
Choice [1/2/3]: 2

✓ wrote deploy/.env
✓ wrote deploy/Caddyfile (mode: http01)
==> docker compose up -d --build

────────────────────────────────────────────────────────────────────────
  Panel
    URL:      https://r.example.com:13337/ui/<random>/
    user:     admin
    password: x9k2pQ8mNvL3rT5w

  Add an agent:
    sudo ./rclient-agent install --connect eyJ1cmwiOiJ3c3M6...

────────────────────────────────────────────────────────────────────────
```

### Agents

On each box you want to manage, run:

```sh
curl -fsSL https://raw.githubusercontent.com/b3s3da/rclient/main/install-agent.sh | sudo sh
```

That fetches the latest release binary for your architecture, verifies the
SHA256, and runs `rclient-agent install`, which asks for the connect blob
from the panel.

You can skip the prompt and pass the blob inline:

```sh
curl -fsSL https://raw.githubusercontent.com/b3s3da/rclient/main/install-agent.sh \
  | sudo sh -s -- --connect eyJ1cmwiOiJ3c3M6...
```

Prefer to inspect the binary first? Grab the matching artifact from
[Releases](../../releases), drop it on the box, and run
`sudo ./rclient-agent install --connect ...` directly.

The box appears in the panel within a couple of seconds, with live metrics
and a shell ready to open.

## Agent CLI

```text
rclient-agent                       run as a daemon (used by the unit)
rclient-agent install [--connect BLOB | --url ... --token ...]
rclient-agent reconfigure [--connect BLOB | --url ... --token ...]
rclient-agent start | stop | restart | status | logs | uninstall
rclient-agent version
```

`status` is a quick one-shot summary (state, configured URL, agent uuid,
last 20 log lines). `logs` follows the journal (or `/var/log/rclient-agent.log`
on OpenRC).

## Security model

- **Endpoint discovery.** Agent and panel paths are randomly generated by
  `setup.sh`. Anything outside those prefixes returns plain 404.
- **Agent auth.** Two layers: a shared bearer token gates the WS upgrade,
  then per-agent enrollment binds an `agent_id` to a server-issued secret.
  A leaked bearer can't impersonate an existing box, only register new ones.
- **Panel auth.** Cookie-based session (HMAC-signed, derived from the panel
  password — rotating the password kills every existing session). Login form
  is throttled per-IP and locks out for ten minutes after ten failures.
- **Transport.** TLS 1.2+ via Caddy, Let's Encrypt cert obtained over
  Cloudflare DNS-01 so 80/443 stay free for whatever else you run.
- **Web defenses.** Strict CSP, `X-Frame-Options: DENY`, `nosniff`,
  `SameSite=Strict; Secure; HttpOnly` cookies, server-side WS Origin check.
- **Audit log.** Every shell open/close and panel login is recorded with
  agent id, shell id, source IP, and exit code on close.

This is not a hardened multi-tenant tool. There is one admin, no roles, no
2FA, command output is not encrypted at rest (it isn't stored). It's
designed for the case "I have a few of my own boxes and want to manage them
without exposing SSH". For larger deployments use a real RMM.

## Development

```sh
# Build everything for the host platform
go build ./...

# Cross-build agents
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o dist/rclient-agent-linux-amd64 ./cmd/agent
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o dist/rclient-agent-linux-arm64 ./cmd/agent

# Run vet
go vet ./...
```

The web panel is plain HTML/CSS/JS embedded into the server binary via
`embed.FS` (`web/static/`). Edit, rebuild, deploy.

## Project layout

```
setup.sh           server bootstrap (curl|sh entry point)
install-agent.sh   agent installer (curl|sudo sh entry point)
cmd/server/        server entrypoint
cmd/agent/         agent entrypoint, install/service subcommands
internal/proto/    JSON message envelope shared by both sides
internal/server/   hub, agent/panel WS handlers, sessions, enrollment
internal/agent/    daemon, metrics collector, PTY shell
web/static/        embedded panel UI
deploy/            Caddyfile templates, docker-compose, Dockerfile
```

## License

MIT — see [LICENSE](LICENSE).
