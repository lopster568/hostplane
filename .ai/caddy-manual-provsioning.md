Here's the complete step-by-step guide of what we did to get a WordPress site working:

Manual WordPress Site Provisioning Guide
Architecture
Browser → Cloudflare (HTTPS/DDoS) → VPS nginx (TCP forward) → WireGuard tunnel → Caddy (Docker, SSL+routing) → PHP-FPM (Docker) → WordPress

Prerequisites

VPS with public IP (157.245.107.34)
app-01 (home server) connected to VPS via WireGuard tunnel (10.0.0.2)
Caddy running as Docker container on app-01
MariaDB running on state-01 (10.10.0.20)
Wildcard DNS \*.cowsaidmoo.tech → 157.245.107.34 (Cloudflare, orange cloud ON)
Cloudflare SSL mode set to Full (Strict)

Step 1: Create Database (on state-01)
bashmariadb -u root -pcontrol@123
sqlCREATE DATABASE IF NOT EXISTS wp_manualtest;
CREATE USER IF NOT EXISTS 'wp_manualtest'@'%' IDENTIFIED BY 'testpass123';
GRANT ALL PRIVILEGES ON wp_manualtest.\* TO 'wp_manualtest'@'%';
FLUSH PRIVILEGES;
EXIT;

```

Expected output:
```

Query OK, 1 row affected (0.000 sec)
Query OK, 0 rows affected (0.003 sec)
Query OK, 0 rows affected (0.001 sec)
Query OK, 0 rows affected (0.000 sec)

Step 2: Create Docker Volume (on app-01)
bashdocker volume create wp_manualtest

```

Expected output:
```

wp_manualtest

Step 3: Create PHP-FPM Container (on app-01)
bashdocker run -d \
 --name php_manualtest \
 --network wp_backend \
 --restart unless-stopped \
 -e WORDPRESS_DB_HOST=10.10.0.20:3306 \
 -e WORDPRESS_DB_USER=wp_manualtest \
 -e WORDPRESS_DB_PASSWORD=testpass123 \
 -e WORDPRESS_DB_NAME=wp_manualtest \
 -v wp_manualtest:/var/www/html \
 wordpress:php8.2-fpm
Verify it's running:
bashdocker ps | grep php_manualtest

```

Expected output:
```

0374dc2a1df6 wordpress:php8.2-fpm "docker-entrypoint.s…" 12 seconds ago Up 12 seconds 9000/tcp php_manualtest

Step 4: Write Caddy Config (on app-01)
bashcat > /opt/caddy/sites/manualtest.caddy << 'EOF'
manualtest.cowsaidmoo.tech {
encode gzip
rewrite / /index.php
reverse_proxy php_manualtest:9000 {
transport fastcgi {
root /var/www/html
split .php
}
}
}
EOF

Step 5: Reload Caddy (on app-01)
bashdocker exec caddy caddy reload --config /etc/caddy/Caddyfile

```

Caddy will automatically obtain a Let's Encrypt SSL cert. Watch for this in the output:
```

certificate obtained successfully identifier: manualtest.cowsaidmoo.tech

Step 6: Verify
bashcurl -skL https://manualtest.cowsaidmoo.tech | head -10
Expected output:
html<!DOCTYPE html>

<html lang="en-US">
<head>
<title>WordPress › Installation</title>
...
```

Or open `https://manualtest.cowsaidmoo.tech` in browser — WordPress installation page loads.

---

### Key Learnings

**The critical Caddy config line is:**

```
rewrite / /index.php
Without this, Caddy passes the directory path /var/www/html to PHP-FPM which rejects it with Access denied. The rewrite ensures all root requests are handled by WordPress's index.php.
Caddy reload command (zero downtime):
bashdocker exec caddy caddy reload --config /etc/caddy/Caddyfile
Control plane writeCaddyConfig template:
goconf := fmt.Sprintf(`%s {
    encode gzip
    rewrite / /index.php
    reverse_proxy %s:9000 {
        transport fastcgi {
            root /var/www/html
            split .php
        }
    }
}
`, hosts, phpName)

Naming Conventions Used by Control Plane
ResourcePatternExampleDatabasewp_<site>wp_manualtestDB Userwp_<site>wp_manualtestVolumewp_<site>wp_manualtestPHP containerphp_<site>php_manualtestCaddy config<site>.caddymanualtest.caddyDomain<site>.cowsaidmoo.techmanualtest.cowsaidmoo.tech
```
