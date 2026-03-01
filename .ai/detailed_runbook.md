# Hostplane — app-01 Setup Runbook

Complete guide to go from a fresh app-01 machine to a working WordPress hosting stack,
and then provision a new site.

---

## Architecture

```
Browser
  → Cloudflare (HTTPS / DDoS)
    → VPS 129.212.247.213 (nginx TCP forwarder)
      → WireGuard tunnel
        → app-01 Caddy (routing by hostname)
          → nginx_<site> (static files + FastCGI proxy)
            → php_<site> (WordPress PHP-FPM)
```

Each WordPress site has three containers:

| Container      | Image                  | Role                                       |
| -------------- | ---------------------- | ------------------------------------------ |
| `php_<site>`   | `wordpress:php8.2-fpm` | PHP-FPM, WordPress files                   |
| `nginx_<site>` | `nginx:alpine`         | Static file serving + FastCGI proxy to FPM |
| `caddy`        | `caddy:latest`         | TLS termination, hostname routing to nginx |

---

## Prerequisites

- app-01 connected to VPS via WireGuard (VPS IP: `129.212.247.213`, app-01 tunnel IP: `10.0.0.2`)
- MariaDB running on state-01 (`10.10.0.20`)
- Wildcard DNS `*.cowsaidmoo.tech` → `129.212.247.213` (Cloudflare, orange cloud ON)
- Cloudflare SSL mode: **Full (Strict)**
- Docker installed on app-01

---

## Part 1 — One-Time Setup (Fresh Machine)

### 1.1 Create Docker network

```bash
docker network create wp_backend
```

### 1.2 Create shared static sites volume

```bash
docker volume create caddy_static_sites
```

### 1.3 Set up Caddy

```bash
mkdir -p /opt/caddy/sites
```

Create `/opt/caddy/Caddyfile`:

```
{
    email admin@cowsaidmoo.tech
}

import /etc/caddy/sites/*.caddy
```

Create `/opt/caddy/docker-compose.yaml`:

```yaml
services:
  caddy:
    image: caddy:latest
    container_name: caddy
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile
      - ./sites:/etc/caddy/sites
      - caddy_static_sites:/srv/sites
      - caddy_data:/data
      - caddy_config:/config
    networks:
      - wp_backend

volumes:
  caddy_data:
  caddy_config:
  caddy_static_sites:
    external: true

networks:
  wp_backend:
    external: true
```

Start Caddy:

```bash
cd /opt/caddy && docker compose up -d
docker ps | grep caddy
```

### 1.4 Set up nginx directory

```bash
mkdir -p /opt/nginx/sites
```

Create `/opt/nginx/docker-compose.yaml`:

```yaml
services: {}
# Services are appended here when a new site is provisioned.

volumes: {}
# Volumes are appended here when a new site is provisioned.

networks:
  wp_backend:
    external: true
```

---

## Part 2 — Provisioning a New WordPress Site

Replace `SITENAME` throughout with your actual site name (lowercase, no dots, e.g. `myshop`).

### Step 1 — Create database (on state-01)

```bash
mariadb -u root -pcontrol@123 -e "
DROP DATABASE IF EXISTS wp_SITENAME;
DROP USER IF EXISTS 'wp_SITENAME'@'%';
CREATE DATABASE wp_SITENAME;
CREATE USER 'wp_SITENAME'@'%' IDENTIFIED BY 'CHANGE_ME';
GRANT ALL PRIVILEGES ON wp_SITENAME.* TO 'wp_SITENAME'@'%';
FLUSH PRIVILEGES;
"
```

### Step 2 — Create Docker volume (on app-01)

```bash
docker volume create wp_SITENAME
```

### Step 3 — Start PHP-FPM container (on app-01)

```bash
docker run -d \
  --name php_SITENAME \
  --network wp_backend \
  --restart unless-stopped \
  -e WORDPRESS_DB_HOST=10.10.0.20:3306 \
  -e WORDPRESS_DB_USER=wp_SITENAME \
  -e WORDPRESS_DB_PASSWORD=CHANGE_ME \
  -e WORDPRESS_DB_NAME=wp_SITENAME \
  -v wp_SITENAME:/var/www/html \
  wordpress:php8.2-fpm

docker ps | grep php_SITENAME
```

### Step 4 — Write nginx config (on app-01)

```bash
cat > /opt/nginx/sites/SITENAME.conf << 'EOF'
server {
    listen 80;
    root /var/www/html;
    index index.php;

    location / {
        try_files $uri $uri/ /index.php?$args;
    }

    location ~ \.php$ {
        fastcgi_pass php_SITENAME:9000;
        fastcgi_index index.php;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
        fastcgi_param HTTPS on;
        fastcgi_param HTTP_HOST SITENAME.cowsaidmoo.tech;
    }
}
EOF
```

### Step 5 — Add nginx service to docker-compose (on app-01)

Append under `services:` in `/opt/nginx/docker-compose.yaml`:

```yaml
nginx_SITENAME:
  image: nginx:alpine
  container_name: nginx_SITENAME
  restart: unless-stopped
  volumes:
    - ./sites/SITENAME.conf:/etc/nginx/conf.d/default.conf
    - wp_SITENAME:/var/www/html
  networks:
    - wp_backend
```

Append under `volumes:` in `/opt/nginx/docker-compose.yaml`:

```yaml
wp_SITENAME:
  external: true
```

Bring up the new container:

```bash
cd /opt/nginx && docker compose up -d nginx_SITENAME
docker ps | grep nginx_SITENAME
```

### Step 6 — Write Caddy site config (on app-01)

```bash
cat > /opt/caddy/sites/SITENAME.caddy << 'EOF'
SITENAME.cowsaidmoo.tech {
    encode gzip
    reverse_proxy nginx_SITENAME:80
}
EOF

docker exec caddy caddy reload --config /etc/caddy/Caddyfile
```

### Step 7 — Verify

```bash
# CSS should return content, not 404
curl -sk https://SITENAME.cowsaidmoo.tech/wp-includes/css/dashicons.min.css | head -3

# Should return WordPress install page HTML
curl -skL https://SITENAME.cowsaidmoo.tech | head -5
```

Then open `https://SITENAME.cowsaidmoo.tech` in a browser — styled WordPress installation wizard should load.

---

## Naming Conventions

| Resource        | Pattern                         | Example                      |
| --------------- | ------------------------------- | ---------------------------- |
| Database        | `wp_<site>`                     | `wp_manualtest`              |
| DB user         | `wp_<site>`                     | `wp_manualtest`              |
| Docker volume   | `wp_<site>`                     | `wp_manualtest`              |
| PHP container   | `php_<site>`                    | `php_manualtest`             |
| nginx container | `nginx_<site>`                  | `nginx_manualtest`           |
| nginx config    | `/opt/nginx/sites/<site>.conf`  | `sites/manualtest.conf`      |
| Caddy config    | `/opt/caddy/sites/<site>.caddy` | `sites/manualtest.caddy`     |
| Domain          | `<site>.cowsaidmoo.tech`        | `manualtest.cowsaidmoo.tech` |

---

## Teardown — Removing a Site

```bash
SITE=SITENAME

docker rm -f php_$SITE nginx_$SITE
docker volume rm wp_$SITE

rm /opt/nginx/sites/$SITE.conf
rm /opt/caddy/sites/$SITE.caddy

# Manually remove the service and volume blocks from /opt/nginx/docker-compose.yaml

docker exec caddy caddy reload --config /etc/caddy/Caddyfile

# On state-01:
mariadb -u root -pcontrol@123 -e "
DROP DATABASE IF EXISTS wp_$SITE;
DROP USER IF EXISTS 'wp_$SITE'@'%';
"
```

---

## Useful Commands

```bash
# All running containers
docker ps --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'

# Caddy logs
docker logs -f caddy

# Reload Caddy after config change
docker exec caddy caddy reload --config /etc/caddy/Caddyfile

# nginx logs for a site
docker logs -f nginx_SITENAME

# PHP-FPM logs for a site
docker logs -f php_SITENAME

# Reload nginx inside sidecar after config change
docker exec nginx_SITENAME nginx -s reload
```
