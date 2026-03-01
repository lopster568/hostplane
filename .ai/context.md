# Hostplane — Context Summary

> **Last updated: 2026-02-24** | `go build` ✅

## What This Is

Go control-plane binary (`/opt/control/control-plane`) running as a systemd service on **control-01** (LXC on Proxmox). Provisions and destroys WordPress sites on **app-01** via the Docker API over TLS. No Cloudflare Tunnel dependency.

---

## Infrastructure

| Host       | Role                                      | IP              |
| ---------- | ----------------------------------------- | --------------- |
| control-01 | control-plane binary, scripts             | LXC internal    |
| app-01     | Docker host — all site containers + Caddy | 10.10.0.10      |
| state-01   | MariaDB — one DB per site                 | 10.10.0.20      |
| VPS        | nginx TCP forwarder only (dumb, no logic) | 129.212.247.213 |

Wildcard DNS `*.cowsaidmoo.tech → 129.212.247.213` (Cloudflare, orange cloud ON, Full Strict).

---

## Request Path

```
Browser → Cloudflare → VPS nginx (TCP fwd) → WireGuard → Caddy (app-01)
  → reverse_proxy nginx_<site>:80
    → nginx_<site>  (static files from volume + fastcgi_pass to FPM)
      → php_<site>:9000  (WordPress PHP-FPM)
        → MariaDB wp_<site> (state-01)
```

---

## Per-Site Resources

| Resource                | Name                                                 | Where    |
| ----------------------- | ---------------------------------------------------- | -------- |
| MariaDB database + user | `wp_<site>`                                          | state-01 |
| Docker volume           | `wp_<site>`                                          | app-01   |
| PHP-FPM container       | `php_<site>`                                         | app-01   |
| nginx sidecar container | `nginx_<site>`                                       | app-01   |
| nginx server block      | inside `nginx_<site>:/etc/nginx/conf.d/default.conf` | app-01   |
| Caddy snippet           | inside `caddy:/etc/caddy/sites/<site>.caddy`         | app-01   |

---

## Key .env (control-01 `/opt/control/.env`)

```
DOCKER_HOST=tcp://10.10.0.10:2376
DOCKER_CERT_DIR=/opt/control/certs
CADDY_CONTAINER=caddy
CADDY_CONF_DIR=/etc/caddy/sites
BASE_DOMAIN=cowsaidmoo.tech
APP_SERVER_IP=10.10.0.10
DOCKER_NETWORK=wp_backend
WP_DSN=control:control@123@tcp(10.10.0.20:3306)/
```

---

## Source Files

| File                    | Role                                                       |
| ----------------------- | ---------------------------------------------------------- |
| `main.go`               | wires everything, starts worker + HTTP server              |
| `api.go`                | HTTP handlers (Gin)                                        |
| `config.go`             | `LoadConfig()` from env                                    |
| `db.go`                 | control-plane DB (jobs, sites)                             |
| `naming.go`             | all resource name functions — only place names are defined |
| `lifecycle.go`          | `SiteStatus` enum + state machine + domain validation      |
| `provisioner.go`        | 7-step WP site provision + rollback                        |
| `destroyer.go`          | reverse of provision                                       |
| `static_provisioner.go` | static site provision                                      |
| `worker.go`             | job poll loop                                              |
| `caddy.go`              | `reloadCaddy()`, `ensureCaddyConfDir()`                    |
| `tunnel.go`             | `TunnelManager` (custom domain routing)                    |

---

## Naming Conventions (from `naming.go`)

```
PHPContainerName(site)   → php_<site>
NginxContainerName(site) → nginx_<site>
VolumeName(site)         → wp_<site>
WPDatabaseName(site)     → wp_<site>
WPDatabaseUser(site)     → wp_<site>
WPDatabasePass(site)     → pass_<site>
CaddyConfFile(site)      → <site>.caddy
NginxConfFile(site)      → <site>.conf
SiteDomain(site, base)   → <site>.<base>
```

---

## Site Status State Machine

```
CREATED → PROVISIONING → ACTIVE → DESTROYING → DESTROYED
                       ↘ FAILED
ACTIVE → DOMAIN_PENDING → DOMAIN_VALIDATING → DOMAIN_ROUTING → DOMAIN_ACTIVE
```

---

## Deploy

```bash
cd /home/oni/hostplane
go build -o control-plane .
cp control-plane /opt/control/control-plane
systemctl restart control-plane
```

---

## Scripts (`/opt/control/scripts/`)

- **`health.sh`** — checks service, MySQL, Docker, Caddy, per-site containers (php* + nginx*), smoke test
- **`cleanup.sh`** — clears dead/zombie sites, stuck/pending jobs, orphaned containers/volumes/Caddy snippets
- **`config.sh`** — sets up `/root/.my-hosto.cnf`

---

## Stale `.ai/` files (superseded, kept for git history only)

- `plan.md` — old architectural principles doc
- `lifecycle-refactor.md` — 943-line audit from before nginx-sidecar architecture
- `caddy-manual-provsioning.md` — old manual guide, replaced by `detailed_runbook.md`

### P3 — Reconciliation loop

12. **Implement `Reconciler` struct** — Background goroutine (separate tick from job worker, e.g. 60s).
13. **Drift detection for ACTIVE sites** — Container running? Nginx config exists? Tunnel route present?
14. **Orphan detection** — Containers/configs/routes without matching DB records.
15. **Auto-repair for safe drifts** — Restart stopped containers, regenerate missing nginx configs.
16. **Replace `health.sh`/`cleanup.sh`** functionality with reconciler + API endpoints.

### P4 — Observability + Testing

17. **Structured logging** — `slog` or `OpContext` wrapper with site/job/step/elapsed on every line.
18. **State machine unit tests** — Exhaustive `CanTransitionTo()` coverage.
19. **Rollback path tests** — Every partial failure scenario with mock providers.
20. **Idempotency tests** — Call every provider method twice, assert identical outcome.
21. **Integration tests** — Full lifecycle provision → domain → destroy with real Docker.
22. **Cloudflare IP range validation** in `ValidateDomainDNS`.

---

## Architecture Invariants (Do Not Violate)

1. **DB is the single source of truth.** Nginx, cloudflared, tunnel routes are derived artifacts.
2. **Validate → Apply Infra → Confirm → Commit DB.** Never write DB before infra succeeds.
3. **State transitions must be explicit.** All status changes go through `TransitionSite()` / `CanTransitionTo()`.
4. **Every operation must be idempotent.** Safe to retry without side effects.
5. **Generate, don't patch.** Config files should be fully regenerated from DB, not incrementally modified.
6. **No fire-and-forget.** Every infra mutation must confirm success synchronously.
7. **Rollback on partial failure.** If step N fails, compensate steps 1..N-1.
