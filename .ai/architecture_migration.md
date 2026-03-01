# Architecture Migration

## Old Architecture (Cloudflare Tunnel)

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

**Why it was abandoned:**

- Cloudflare Tunnel required a restart for every new site route
- SSL was fully tunnel-dependent — no tunnel, no HTTPS
- Custom domains required customers to be on Cloudflare
- Frequent bad gateways and tunnel instability
- Control plane tightly coupled to cloudflared lifecycle

---

## Current Architecture

```
Customer browser
    → Cloudflare (DDoS protection, HTTPS termination)
        → VPS 129.212.247.213 (nginx — dumb TCP forwarder, no logic)
            → WireGuard encrypted tunnel
                → app-01 Caddy (routes by hostname)
                    → nginx_<site> (static files + FastCGI proxy)
                        → php_<site> (WordPress PHP-FPM)

api.cowsaidmoo.tech
    → Cloudflare → VPS → WireGuard → Caddy → control-01 (LXC on Proxmox)
```

### Per-site container stack

Each WordPress site runs three units:

| Unit           | Type             | Role                                             |
| -------------- | ---------------- | ------------------------------------------------ |
| `php_<site>`   | Docker container | WordPress PHP-FPM, owns the `wp_<site>` volume   |
| `nginx_<site>` | Docker container | nginx sidecar — serves static files, proxies PHP |
| `wp_<site>`    | Docker volume    | WordPress files, mounted into both containers    |

### Why nginx sidecar instead of Caddy FastCGI

Caddy can serve WordPress static files only if the WordPress volume is mounted into it directly.
With one site that's tolerable; with many sites it means mounting every `wp_<site>` volume into
the single Caddy container — fragile, requires Caddy restarts, and breaks the isolation model.

The nginx sidecar approach gives each site its own nginx container with its own volume mount.
Caddy never touches PHP or static files — it only reverse proxies by hostname. Adding a new site
never requires changes to the Caddy container itself.

### Caddy config (per site)

Written to `/opt/caddy/sites/<site>.caddy` by the control plane:

```
mysite.cowsaidmoo.tech {
    encode gzip
    reverse_proxy nginx_mysite:80
}
```

### nginx sidecar config (per site)

Written to `/opt/nginx/sites/<site>.conf`, mounted into `nginx_<site>`:

```nginx
server {
    listen 80;
    root /var/www/html;
    index index.php;

    location / {
        try_files $uri $uri/ /index.php?$args;
    }

    location ~ \.php$ {
        fastcgi_pass php_mysite:9000;
        fastcgi_index index.php;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
        fastcgi_param HTTPS on;
        fastcgi_param HTTP_HOST mysite.cowsaidmoo.tech;
    }
}
```

---

## New Site Provisioning Flow

```
Control plane receives CreateSite request
    → Creates database + user on state-01 (MariaDB, 10.10.0.20)
    → Creates Docker volume wp_<site> on app-01
    → Starts php_<site> container (wordpress:php8.2-fpm, mounts wp_<site>)
    → Writes /opt/nginx/sites/<site>.conf via Docker CopyToContainer
    → Adds nginx_<site> service + wp_<site> volume to /opt/nginx/docker-compose.yaml
    → Runs: docker compose up -d nginx_<site>
    → Writes /opt/caddy/sites/<site>.caddy via Docker CopyToContainer
    → Runs: docker exec caddy caddy reload --config /etc/caddy/Caddyfile
    → Site is live at <site>.cowsaidmoo.tech (wildcard DNS catches it instantly)
```

No DNS changes. No Caddy container restarts. No volume remounts. Each step is isolated.

---

## Infrastructure Layout

### app-01

```
/opt/caddy/
├── docker-compose.yaml      # Caddy container (ports 80/443)
├── Caddyfile                # global opts + import /etc/caddy/sites/*.caddy
└── sites/                   # per-site caddy snippets written by control plane
    └── <site>.caddy

/opt/nginx/
├── docker-compose.yaml      # one service block per site appended here
└── sites/                   # per-site nginx configs written by control plane
    └── <site>.conf
```

### control-01 (LXC container on Proxmox)

```
/opt/control/
├── control-plane            # compiled Go binary, runs as systemd service
├── .env                     # EnvironmentFile for systemd unit
└── certs/                   # Docker TLS client certs to reach app-01 daemon
    ├── ca.pem
    ├── cert.pem
    └── key.pem
```

Build and deploy:

```bash
cd /home/oni/hostplane
go build -o control-plane .
cp control-plane /opt/control/control-plane
systemctl restart control-plane
```

### Key .env vars (control-01)

```
DOCKER_HOST=tcp://10.10.0.10:2376
DOCKER_CERT_DIR=/opt/control/certs
CADDY_CONTAINER=caddy
CADDY_CONF_DIR=/etc/caddy/sites
NGINX_CONF_DIR=/opt/nginx/sites
NGINX_COMPOSE_FILE=/opt/nginx/docker-compose.yaml
BASE_DOMAIN=cowsaidmoo.tech
APP_SERVER_IP=10.10.0.10
DOCKER_NETWORK=wp_backend
DB_HOST=10.10.0.20:3306
DB_ROOT_PASSWORD=control@123
```

---

## What Each Layer Does

| Layer             | Role                                                                   |
| ----------------- | ---------------------------------------------------------------------- |
| **Cloudflare**    | HTTPS termination, DDoS protection, hides VPS IP                       |
| **VPS nginx**     | Dumb TCP forwarder — no SSL, no logic, passes raw bytes                |
| **WireGuard**     | Encrypted tunnel between VPS (129.212.247.213) and app-01 (10.10.0.10) |
| **Caddy**         | Routes by hostname to the correct `nginx_<site>` sidecar               |
| **nginx\_<site>** | Serves WordPress static assets, proxies PHP requests to FPM            |
| **php\_<site>**   | WordPress PHP-FPM, owns the site's files and database connection       |
| **state-01**      | MariaDB — one database per site                                        |
| **control-01**    | Go control plane binary — provisions/destroys sites via Docker API     |

---

## Key Improvements Over Old Architecture

- No Cloudflare Tunnel dependency — VPS is a €4/month dumb gateway
- Wildcard DNS `*.cowsaidmoo.tech` — zero DNS changes per new site
- Custom domains only need a CNAME to `cowsaidmoo.tech`
- Caddy reload is instantaneous, zero downtime per site add
- nginx sidecar scales to any number of sites without touching Caddy's container
- Full isolation: each site's volume is mounted only into its own containers
- All compute runs on home hardware; VPS is purely network ingress

---

## Outstanding Work

- [ ] Custom domain flow — customer adds CNAME, control plane writes extra Caddy block
- [ ] Site destroy via control plane (containers, volume, DB, config files, compose cleanup)
- [ ] Static site support (non-WordPress) — nginx sidecar serves from its own volume
- [ ] HTTPS health check endpoint per site
- [ ] Remove old cloudflared config once confirmed fully decommissioned
