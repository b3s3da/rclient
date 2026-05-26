# Contributing

Thanks for taking a look. The project is small, so the rules are simple.

## Getting set up

```sh
git clone https://github.com/b3s3da/rclient
cd rclient
go build ./...    # everything should build on a stock Go toolchain
go vet ./...
```

The web panel lives in `web/static/` and is embedded into the server
binary at compile time via `embed.FS`. Edit, rebuild, run.

## Local end-to-end test (no TLS, no Docker)

Two terminals.

Server:

```sh
mkdir -p ./tmp/server
RCLIENT_LISTEN=:8080 \
RCLIENT_AGENT_PATH=/ws/dev \
RCLIENT_PANEL_PATH=/ui/dev \
RCLIENT_AGENT_TOKEN=devdevdev \
RCLIENT_PANEL_USER=admin \
RCLIENT_PANEL_PASS=devdev \
RCLIENT_ENROLL_PATH=./tmp/server/enroll.json \
go run ./cmd/server
```

Agent (in another terminal):

```sh
mkdir -p ./tmp/agent
go run ./cmd/agent \
  --url ws://127.0.0.1:8080/ws/dev \
  --token devdevdev \
  --state ./tmp/agent
```

Browser: <http://127.0.0.1:8080/ui/dev/> — log in with `admin` / `devdev`.

The session cookie is normally `Secure`-only; for the plain-HTTP dev flow
you'll need to either trust localhost in your browser or run a tiny TLS
proxy (`caddy reverse-proxy --to :8080 --from localhost`).

## Code style

- Plain Go, stdlib first. Third-party deps need a clear reason.
- Comments explain intent, not mechanics. Code should be readable enough
  that "what" doesn't need narration.
- Keep packages flat where possible. We don't need a 5-level hierarchy.
- Web panel is plain HTML/CSS/JS — no build step, no framework. New
  features should preserve that.

## What welcome patches look like

- Bug fixes with a short reproduction in the PR description.
- Improvements to the install/uninstall flow on supported distros.
- New agent metrics, as long as they don't pull a heavy dependency.
- Tightening of security headers, audit logging, throttling.
- Additional Caddyfile templates for other DNS-01 providers (DuckDNS,
  Hetzner, Route53, etc.) — drop a `Caddyfile.dns-<name>` and a
  `compose.dns-<name>.yml`, wire them into `setup.sh`.

## What is harder to get merged

- Adding mandatory dependencies (sqlite, embedded JS frameworks, etc.).
- Multi-tenant features (roles, multiple users) — not the project's goal.
- Agent capabilities that significantly change the security posture
  (file upload, package installer, etc.) without a discussion first.

Open an issue first for anything bigger than a small fix.
