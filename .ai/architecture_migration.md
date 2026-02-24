Here's the full architecture summary:

**Old Architecture:**

```
Customer browser
    → Cloudflare (proxied)
        → Cloudflare Tunnel (cloudflared)
            → app-01 nginx (site routing)
                → Docker containers (WordPress/static sites)

Control plane added new sites by:
    → Writing nginx config
    → Adding route to cloudflared tunnel config
    → Restarting tunnel
```

**Problems with old architecture:**

- Cloudflare tunnel required for every new site route
- SSL was tunnel-dependent
- Custom domains required customers to be on Cloudflare
- Bad gateways and tunnel instability
- Control plane tightly coupled to cloudflared

---

**New Architecture:**

```
Customer browser
    → Cloudflare (DDoS protection, HTTPS)
        → VPS 157.245.107.34 (nginx TCP forwarder)
            → WireGuard encrypted tunnel
                → app-01 Caddy (Docker container, site routing)
                    → Docker containers (WordPress/static sites)

api.cowsaidmoo.tech
    → Cloudflare → VPS → WireGuard → Caddy → control-01 (LXC, Proxmox internal)
```

**New site onboarding flow:**

```
Control plane creates site
    → Spins up Docker container on app-01 (WordPress: php_<site> / Static: none)
    → Writes /opt/caddy/sites/<site>.caddy via Docker CopyToContainer
    → Runs: docker exec caddy caddy reload --config /etc/caddy/Caddyfile
    → Site instantly live at newsite.cowsaidmoo.tech (wildcard DNS catches it)
```

**Caddy config layout on app-01:**

```
/opt/caddy/
├── docker-compose.yaml      # caddy container (bind-mounts ./sites and ./Caddyfile)
├── Caddyfile                # global config + import line
└── sites/                   # per-site snippets written by control plane
    ├── mysite.caddy
    └── shop.caddy
```

Caddyfile must contain:
```
import /etc/caddy/sites/*.caddy
```

Per-site snippet — WordPress:
```
mysite.cowsaidmoo.tech {
    root * /var/www/html
    php_fastcgi php_mysite:9000
    file_server
    encode gzip
}
```

Per-site snippet — Static:
```
mysite.cowsaidmoo.tech {
    root * /srv/sites/mysite
    file_server
    encode gzip
}
```

**Static site file storage:**

- Named Docker volume `caddy_static_sites` mounted at `/srv/sites` in the caddy container
- Control plane extracts uploaded zip into volume under `/{site}/` via temporary busybox container
- No per-site nginx containers — Caddy's `file_server` serves directly

**What each layer does:**

- **Cloudflare** — HTTPS termination, DDoS protection, hides VPS IP
- **VPS nginx** — dumb TCP forwarder, no SSL, no logic, just passes bytes
- **WireGuard** — encrypted tunnel between VPS and app-01
- **Caddy (Docker container on app-01)** — routes by hostname to correct PHP-FPM container or static files
- **Docker containers** — WordPress PHP-FPM sites on `wp_backend` network
- **control-01 (LXC)** — control plane binary runs as **systemd service** (`/opt/control/control-plane`). No Docker containerisation.

**Control plane deployment (control-01):**

```
/opt/control/
├── control-plane       # compiled Go binary
├── .env                # EnvironmentFile for systemd unit
├── certs/              # Docker TLS client certs to reach app-01 daemon
│   ├── ca.pem
│   ├── cert.pem
│   └── key.pem
└── cloudflared/
    └── config.yml      # read/written by TunnelManager
```

Build & deploy:
```bash
cd /home/oni/hostplane
go build -o control-plane .
cp control-plane /opt/control/control-plane
systemctl restart control-plane
```

**Key .env vars (control-01 /opt/control/.env):**

```
DOCKER_HOST=tcp://10.10.0.10:2376
DOCKER_CERT_DIR=/opt/control/certs
CADDY_CONTAINER=caddy
CADDY_CONF_DIR=/etc/caddy/sites
CADDY_STATIC_VOLUME=caddy_static_sites
BASE_DOMAIN=cowsaidmoo.tech
APP_SERVER_IP=10.10.0.10
DOCKER_NETWORK=wp_backend
```

**Key improvements over old architecture:**

- No Cloudflare tunnel dependency
- Wildcard DNS `*.cowsaidmoo.tech` — zero DNS changes per new site
- Custom domains just need a CNAME to `cowsaidmoo.tech`
- Caddy reload is instant, zero downtime
- No per-site nginx containers for static sites
- Home hardware does all compute, VPS is just a €4/month gateway

**Still to do:**

- Mount `caddy_static_sites` volume into caddy container on app-01
- Custom domain flow for customers (CNAME → wildcard)
- Remove old Cloudflare tunnel once confirmed stable
