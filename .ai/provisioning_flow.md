# Provisioning Flow — What Gets Created Per Site

## Trigger

```
POST /api/sites
body: site=mysite
```

The API validates the name (lowercase alphanum, not already in DB), inserts a `sites` row
(`status=PROVISIONING`) and a `jobs` row (`type=PROVISION, status=PENDING`), and returns
`202 Accepted` immediately. Nothing on app-01 has been touched yet.

---

## Step 1 — Worker claims the job (control-01)

The worker polls the DB every 3 seconds. It atomically claims the next `PENDING` job
(`status → PROCESSING`) and calls `provisioner.Run("mysite")`.

---

## Step 2 — Database (state-01 · 10.10.0.20)

```sql
CREATE DATABASE wp_mysite;
CREATE USER 'wp_mysite'@'%' IDENTIFIED BY 'pass_mysite';
GRANT ALL PRIVILEGES ON wp_mysite.* TO 'wp_mysite'@'%';
FLUSH PRIVILEGES;
```

WordPress reads these credentials from env vars when it starts. If this step fails,
nothing else exists yet — rollback is a no-op.

---

## Step 3 — Docker volume (app-01 · 10.10.0.10)

```
docker volume create wp_mysite
```

This is the shared filesystem both containers mount. `php_mysite` writes WordPress
files into it on first boot; `nginx_mysite` reads from it to serve static assets.

---

## Step 4 — PHP-FPM container (app-01)

```
Name:    php_mysite
Image:   wordpress:php8.2-fpm
Network: wp_backend
Volume:  wp_mysite → /var/www/html  (read-write)
Env:     WORDPRESS_DB_HOST=10.10.0.20:3306
         WORDPRESS_DB_USER=wp_mysite
         WORDPRESS_DB_PASSWORD=pass_mysite
         WORDPRESS_DB_NAME=wp_mysite
Limits:  512 MB RAM · 1 CPU · 100 pids
Restart: unless-stopped
```

On first start the official WordPress image bootstraps its files into `wp_mysite`.
All PHP execution for this site happens here on port 9000.

---

## Step 5 — nginx sidecar container (app-01)

```
Name:    nginx_mysite
Image:   nginx:alpine
Network: wp_backend
Volume:  wp_mysite → /var/www/html  (read-only)
Limits:  128 MB RAM · 0.5 CPU · 50 pids
Restart: unless-stopped
```

Mounts the **same** `wp_mysite` volume as the PHP container, read-only. nginx serves
static assets (JS, CSS, images, fonts) directly from disk and proxies PHP requests
to `php_mysite:9000` via FastCGI.

---

## Step 6 — nginx server block injected into the sidecar (app-01)

Written via `docker cp` (tar stream) into `nginx_mysite:/etc/nginx/conf.d/default.conf`,
then `docker exec nginx_mysite nginx -s reload` applies it live:

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

## Step 7 — Caddy snippet (app-01, inside caddy container)

Written via `docker cp` into `caddy:/etc/caddy/sites/mysite.caddy`:

```
mysite.cowsaidmoo.tech {
    encode gzip
    reverse_proxy nginx_mysite:80
}
```

Caddy never speaks FastCGI. It routes by hostname to the nginx sidecar and nothing else.

---

## Step 8 — Caddy reload (app-01)

```
docker exec caddy caddy reload --config /etc/caddy/Caddyfile
```

Zero downtime. The `import /etc/caddy/sites/*.caddy` in the global Caddyfile picks
up the new snippet instantly. **Site is live.**

---

## Request path after provisioning

```
Browser
  → Cloudflare (HTTPS termination, DDoS protection)
    → VPS 129.212.247.213 — nginx TCP forwarder (dumb, no logic)
      → WireGuard tunnel
        → Caddy on app-01
            matches hostname: mysite.cowsaidmoo.tech
            → reverse_proxy nginx_mysite:80
              → nginx_mysite
                  static file?  → served from wp_mysite volume directly
                  .php request? → fastcgi_pass php_mysite:9000
                    → php_mysite (WordPress PHP-FPM)
                      ↕
                    → state-01 MariaDB — database wp_mysite
```

---

## Resources created — summary

| Resource                | Name                                                 | Location |
| ----------------------- | ---------------------------------------------------- | -------- |
| MariaDB database        | `wp_mysite`                                          | state-01 |
| MariaDB user            | `wp_mysite`                                          | state-01 |
| Docker volume           | `wp_mysite`                                          | app-01   |
| PHP-FPM container       | `php_mysite`                                         | app-01   |
| nginx sidecar container | `nginx_mysite`                                       | app-01   |
| nginx server block      | inside `nginx_mysite:/etc/nginx/conf.d/default.conf` | app-01   |
| Caddy snippet           | inside `caddy:/etc/caddy/sites/mysite.caddy`         | app-01   |

No DNS changes. No Caddy container restarts. No impact on any other running site.

---

## Rollback

If any step fails, the provisioner undoes everything created so far in reverse order:

1. Remove Caddy snippet + reload Caddy
2. Stop + remove `nginx_mysite`
3. Stop + remove `php_mysite`
4. Remove volume `wp_mysite`
5. `DROP DATABASE wp_mysite; DROP USER wp_mysite`

The job is marked `FAILED`. The worker will retry up to `max_attempts` times before
giving up. A site stuck in `PROVISIONING` with a `FAILED` job is a "zombie" and can
be cleared with `cleanup.sh`.
