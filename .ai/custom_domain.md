# Custom Domain Flow — Runbook & Control Plane Spec

Complete reference for adding a custom domain to an existing WordPress site,
and auto-configuring WordPress so the customer never sees the subdomain.

---

## How It Works

```
Customer sets A record: phoenixdns.app → 157.245.107.34
Control plane API called: POST /sites/testsite01/domain { "domain": "phoenixdns.app" }
    → Verify A record points to 157.245.107.34
    → Rewrite /opt/caddy/sites/testsite01.caddy with both hostnames
    → Reload Caddy → Let's Encrypt cert issued automatically for phoenixdns.app
    → Rewrite /opt/nginx/sites/testsite01.conf with server_name + $host
    → Reload nginx sidecar
    → Update wp_options: siteurl + home → https://phoenixdns.app
    → Return 200
```

No DNS changes on your side. No Caddy container restarts. Cert is issued in seconds.

---

## Part 1 — Manual Steps (Verified Working)

### Prerequisites

- Site is ACTIVE at `<site>.cowsaidmoo.tech`
- Customer has set an A record: `customdomain.com → 157.245.107.34` (Cloudflare proxy OFF)
- WordPress is installed on the site (or will be auto-installed — see Part 2)

---

### Step 1 — Verify DNS is pointing correctly

Run this from control-01 or any external machine:

```bash
dig +short phoenixdns.app
```

Expected output:

```
157.245.107.34
```

If it returns Cloudflare IPs (`104.x.x.x` / `172.x.x.x`) the proxy is still on — tell customer
to turn off the orange cloud in Cloudflare for that record.

Do NOT proceed until this returns `157.245.107.34`.

---

### Step 2 — Update Caddy config (on app-01)

Rewrite the site's Caddy snippet to include both the subdomain and the custom domain:

```bash
cat > /opt/caddy/sites/testsite01.caddy << 'EOF'
testsite01.cowsaidmoo.tech, phoenixdns.app {
    encode gzip
    reverse_proxy nginx_testsite01:80
}
EOF
```

Reload Caddy (zero downtime):

```bash
docker exec caddy caddy reload --config /etc/caddy/Caddyfile
```

Caddy will immediately begin the ACME HTTP-01 challenge for `phoenixdns.app`.
Watch for cert issuance in logs:

```bash
docker logs caddy --since 30s | grep phoenixdns
```

Expected log lines (within ~10 seconds):

```
tls.obtain  acquiring lock          identifier: phoenixdns.app
tls.obtain  certificate obtained successfully  identifier: phoenixdns.app
```

---

### Step 3 — Update nginx sidecar config (on app-01)

Rewrite the nginx config to accept both hostnames and use `$host` so WordPress
receives the correct domain on each request:

```bash
cat > /opt/nginx/sites/testsite01.conf << 'EOF'
server {
    listen 80;
    root /var/www/html;
    index index.php;

    server_name testsite01.cowsaidmoo.tech phoenixdns.app;

    location / {
        try_files $uri $uri/ /index.php?$args;
    }

    location ~ \.php$ {
        fastcgi_pass php_testsite01:9000;
        fastcgi_index index.php;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
        fastcgi_param HTTPS on;
        fastcgi_param HTTP_HOST $host;
    }
}
EOF
```

Key change: `HTTP_HOST` is `$host` (not hardcoded) — this makes WordPress use
whatever domain the request arrived on.

Reload nginx sidecar:

```bash
docker exec nginx_testsite01 nginx -s reload
```

---

### Step 4 — Update WordPress site URL in database (on state-01)

WordPress hardcodes `siteurl` and `home` in `wp_options`. Until these are updated,
WordPress will redirect all requests to the original subdomain.

```bash
mariadb -u root -pcontrol@123 wp_testsite01 -e "
UPDATE wp_options SET option_value='https://phoenixdns.app' WHERE option_name='siteurl';
UPDATE wp_options SET option_value='https://phoenixdns.app' WHERE option_name='home';
"
```

Verify:

```bash
mariadb -u root -pcontrol@123 wp_testsite01 -e "
SELECT option_name, option_value FROM wp_options
WHERE option_name IN ('siteurl', 'home');
"
```

Expected output:

```
+-------------+------------------------+
| option_name | option_value           |
+-------------+------------------------+
| home        | https://phoenixdns.app |
| siteurl     | https://phoenixdns.app |
+-------------+------------------------+
```

---

### Step 5 — Verify end to end

```bash
# Should return WordPress HTML, no redirects to subdomain
curl -skL https://phoenixdns.app | head -5

# CSS should load (not 404)
curl -sk https://phoenixdns.app/wp-includes/css/dashicons.min.css | head -3
```

Open `https://phoenixdns.app` in browser — site loads on custom domain with valid HTTPS.

---

## Part 2 — Auto WordPress Installation (WP-CLI)

To prevent the customer ever seeing the subdomain install wizard, the control plane
should auto-install WordPress immediately after provisioning, before the customer
visits the site.

### Install WP-CLI into the PHP-FPM container

WP-CLI runs inside the `php_<site>` container:

```bash
docker exec php_testsite01 bash -c "
curl -sO https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar &&
chmod +x wp-cli.phar &&
mv wp-cli.phar /usr/local/bin/wp
"
```

Verify:

```bash
docker exec php_testsite01 wp --info --allow-root
```

---

### Run WordPress auto-install

```bash
docker exec php_testsite01 wp core install \
  --url="https://phoenixdns.app" \
  --title="My Site" \
  --admin_user="admin" \
  --admin_password="CHANGE_ME_STRONG_PASSWORD" \
  --admin_email="admin@phoenixdns.app" \
  --allow-root
```

Expected output:

```
Success: WordPress installed successfully.
```

This does everything in one command:

- Creates all WordPress database tables
- Sets `siteurl` and `home` to the provided `--url`
- Creates the admin user
- Skips the install wizard entirely

---

### Verify admin login works

```bash
curl -skL https://phoenixdns.app/wp-admin/ | head -5
```

Should return the login page HTML, not a redirect to the install wizard.

---

## Part 3 — Control Plane Implementation Spec

### API endpoint

```
POST /sites/{site}/domain
Body: { "domain": "phoenixdns.app" }
```

### Steps the control plane must execute in order

```
1. DNS check
   → net.LookupHost(domain)
   → Assert one of the IPs == "157.245.107.34"
   → If not: return 400 "A record not pointing to 157.245.107.34"

2. Write Caddy config
   → Read /opt/caddy/sites/<site>.caddy via Docker CopyFromContainer
   → Rewrite first line: "<subdomain>, <customdomain> {"
   → Write back via Docker CopyToContainer
   → docker exec caddy caddy reload --config /etc/caddy/Caddyfile

3. Write nginx config
   → Read /opt/nginx/sites/<site>.conf via Docker CopyFromContainer
   → Add/replace server_name line
   → Replace fastcgi_param HTTP_HOST <hardcoded> with fastcgi_param HTTP_HOST $host
   → Write back via Docker CopyToContainer
   → docker exec nginx_<site> nginx -s reload

4. Update WordPress database
   → SQL via MariaDB connection:
     UPDATE wp_options SET option_value='https://<domain>' WHERE option_name='siteurl';
     UPDATE wp_options SET option_value='https://<domain>' WHERE option_name='home';

5. Update site record
   → Set CustomDomain = domain in controlplane DB
   → Set UpdatedAt = now()

6. Return 200 { "domain": "phoenixdns.app", "status": "active" }
```

### Go config templates

**Caddy snippet:**

```go
conf := fmt.Sprintf(`%s, %s {
    encode gzip
    reverse_proxy nginx_%s:80
}
`, subdomain, customDomain, site)
```

**nginx config:**

```go
conf := fmt.Sprintf(`server {
    listen 80;
    root /var/www/html;
    index index.php;

    server_name %s %s;

    location / {
        try_files $uri $uri/ /index.php?$args;
    }

    location ~ \.php$ {
        fastcgi_pass php_%s:9000;
        fastcgi_index index.php;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
        fastcgi_param HTTPS on;
        fastcgi_param HTTP_HOST $host;
    }
}
`, subdomain, customDomain, site)
```

**WP-CLI auto-install (run during site provisioning, not domain add):**

```go
cmd := fmt.Sprintf(
    "wp core install --url=%s --title=%s --admin_user=%s --admin_password=%s --admin_email=%s --allow-root",
    url, title, adminUser, adminPass, adminEmail,
)
// docker exec php_<site> bash -c cmd
```

### WordPress DB update (Go)

```go
db.Exec(`
    UPDATE wp_options SET option_value=? WHERE option_name='siteurl';
    UPDATE wp_options SET option_value=? WHERE option_name='home';
`, "https://"+domain, "https://"+domain)
```

---

## Naming Reference

| Resource        | Pattern                         | Example            |
| --------------- | ------------------------------- | ------------------ |
| Caddy config    | `/opt/caddy/sites/<site>.caddy` | `testsite01.caddy` |
| nginx config    | `/opt/nginx/sites/<site>.conf`  | `testsite01.conf`  |
| WordPress DB    | `wp_<site>`                     | `wp_testsite01`    |
| PHP container   | `php_<site>`                    | `php_testsite01`   |
| nginx container | `nginx_<site>`                  | `nginx_testsite01` |

---

## Removing a Custom Domain

To revert back to subdomain only:

```bash
# Rewrite Caddy config back to subdomain only
cat > /opt/caddy/sites/testsite01.caddy << 'EOF'
testsite01.cowsaidmoo.tech {
    encode gzip
    reverse_proxy nginx_testsite01:80
}
EOF
docker exec caddy caddy reload --config /etc/caddy/Caddyfile

# Revert nginx HTTP_HOST
# (update server_name and HTTP_HOST $host back to hardcoded subdomain)

# Revert WordPress URLs
mariadb -u root -pcontrol@123 wp_testsite01 -e "
UPDATE wp_options SET option_value='https://testsite01.cowsaidmoo.tech' WHERE option_name='siteurl';
UPDATE wp_options SET option_value='https://testsite01.cowsaidmoo.tech' WHERE option_name='home';
"
```
