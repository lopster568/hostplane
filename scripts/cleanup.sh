#!/bin/bash

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

MYSQL="mysql --defaults-file=/root/.my-hosto.cnf -se"
DOCKER="docker --host tcp://10.10.0.10:2376 --tlsverify \
  --tlscacert /opt/control/certs/ca.pem \
  --tlscert /opt/control/certs/cert.pem \
  --tlskey /opt/control/certs/key.pem"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  HOSTO — CLEANUP"
echo "  $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# ── 1. Hard delete destroyed / failed sites ───────────────────
echo ""
echo "[ DESTROYED / FAILED SITES ]"

DEAD_SITES=$($MYSQL "SELECT site FROM sites WHERE status IN ('DESTROYED','FAILED');")

if [ -z "$DEAD_SITES" ]; then
  echo -e "${GREEN}✓${NC} No destroyed/failed sites to clean up"
else
  echo "Sites to remove:"
  for site in $DEAD_SITES; do
    STATUS=$($MYSQL "SELECT status FROM sites WHERE site='$site';")
    CUSTOM=$($MYSQL "SELECT COALESCE(custom_domain,'') FROM sites WHERE site='$site';")
    echo "  - $site ($STATUS)$([ -n "$CUSTOM" ] && echo " (custom: $CUSTOM)")"
  done
  echo ""
  read -p "Delete all destroyed/failed sites and their jobs from DB? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    for site in $DEAD_SITES; do
      CUSTOM=$($MYSQL "SELECT COALESCE(custom_domain,'') FROM sites WHERE site='$site';")

      mysql --defaults-file=/root/.my-hosto.cnf controlplane -e "
        DELETE FROM jobs WHERE site='$site';
        DELETE FROM sites WHERE site='$site';
      "
      echo -e "${GREEN}✓${NC} Deleted $site from DB"

      if [ -n "$CUSTOM" ]; then
        sed -i "/hostname: $CUSTOM/,+1d" /etc/cloudflared/config.yml
        echo -e "${GREEN}✓${NC} Removed $CUSTOM from tunnel config"
        RESTART_CLOUDFLARED=1
      fi
    done

    if [ -n "$RESTART_CLOUDFLARED" ]; then
      systemctl restart cloudflared
      echo -e "${GREEN}✓${NC} Cloudflared restarted"
    fi
  else
    echo "Skipped."
  fi
fi

# ── 2. Orphaned containers ────────────────────────────────────
echo ""
echo "[ ORPHANED CONTAINERS ]"

ALL_CONTAINERS=$($DOCKER ps --format '{{.Names}}' | grep -E '^(php_|static_)')
ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status='ACTIVE';")

ORPHANED=""
for container in $ALL_CONTAINERS; do
  site=${container#php_}
  site=${site#static_}
  if ! echo "$ACTIVE_SITES" | grep -qw "$site"; then
    ORPHANED="$ORPHANED $container"
  fi
done

if [ -z "$ORPHANED" ]; then
  echo -e "${GREEN}✓${NC} No orphaned containers"
else
  echo -e "${YELLOW}!${NC} Orphaned containers found:$ORPHANED"
  read -p "Remove them? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    for container in $ORPHANED; do
      $DOCKER stop "$container" 2>/dev/null
      $DOCKER rm "$container" 2>/dev/null
      echo -e "${GREEN}✓${NC} Removed $container"
    done
  else
    echo "Skipped."
  fi
fi

# ── 3. Orphaned Caddy snippet configs ─────────────────────────
echo ""
echo "[ ORPHANED CADDY CONFIGS ]"

# List all .caddy snippets in /etc/caddy/sites/ inside the container
ALL_SNIPPETS=$($DOCKER exec caddy sh -c 'ls /etc/caddy/sites/*.caddy 2>/dev/null' | xargs -n1 basename 2>/dev/null | sed 's/\.caddy$//')
ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status='ACTIVE';")

ORPHANED_CONF=""
for site in $ALL_SNIPPETS; do
  if ! echo "$ACTIVE_SITES" | grep -qw "$site"; then
    ORPHANED_CONF="$ORPHANED_CONF $site"
  fi
done

if [ -z "$ORPHANED_CONF" ]; then
  echo -e "${GREEN}✓${NC} No orphaned Caddy snippets"
else
  echo -e "${YELLOW}!${NC} Orphaned Caddy snippets found:$ORPHANED_CONF"
  read -p "Remove them and reload Caddy? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    for site in $ORPHANED_CONF; do
      $DOCKER exec caddy rm -f "/etc/caddy/sites/${site}.caddy"
      echo -e "${GREEN}✓${NC} Removed ${site}.caddy"
    done
    $DOCKER exec caddy caddy reload --config /etc/caddy/Caddyfile
    echo -e "${GREEN}✓${NC} Caddy reloaded"
  else
    echo "Skipped."
  fi
fi

# ── 4. Orphaned tunnel config entries ─────────────────────────
echo ""
echo "[ TUNNEL CONFIG ]"

TUNNEL_DOMAINS=$(grep "hostname:" /etc/cloudflared/config.yml | \
  grep -v "cowsaidmoo.tech" | \
  grep -v "api\." | \
  awk '{print $3}' | tr -d "'")

if [ -z "$TUNNEL_DOMAINS" ]; then
  echo -e "${GREEN}✓${NC} No custom domains in tunnel config"
else
  for domain in $TUNNEL_DOMAINS; do
    EXISTS=$($MYSQL "SELECT COUNT(*) FROM sites WHERE custom_domain='$domain' AND status='ACTIVE';")
    if [ "$EXISTS" = "0" ]; then
      echo -e "${YELLOW}!${NC} $domain in tunnel but no active site"
      read -p "Remove $domain from tunnel config? [y/N] " confirm
      if [ "$confirm" = "y" ]; then
        sed -i "/hostname: $domain/,+1d" /etc/cloudflared/config.yml
        echo -e "${GREEN}✓${NC} Removed $domain"
        RESTART_CLOUDFLARED=1
      fi
    else
      echo -e "${GREEN}✓${NC} $domain active"
    fi
  done

  if [ -n "$RESTART_CLOUDFLARED" ]; then
    systemctl restart cloudflared
    echo -e "${GREEN}✓${NC} Cloudflared restarted"
  fi
fi

# ── 5. Old failed jobs ────────────────────────────────────────
echo ""
echo "[ OLD FAILED JOBS ]"

OLD_FAILED=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='FAILED' AND updated_at < NOW() - INTERVAL 7 DAY;")

if [ "$OLD_FAILED" = "0" ]; then
  echo -e "${GREEN}✓${NC} No old failed jobs"
else
  echo -e "${YELLOW}!${NC} $OLD_FAILED failed job(s) older than 7 days"
  read -p "Delete them? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    mysql --defaults-file=/root/.my-hosto.cnf controlplane -e \
      "DELETE FROM jobs WHERE status='FAILED' AND updated_at < NOW() - INTERVAL 7 DAY;"
    echo -e "${GREEN}✓${NC} Deleted $OLD_FAILED old failed jobs"
  else
    echo "Skipped."
  fi
fi

# ── 6. Orphaned tmp zips ──────────────────────────────────────
echo ""
echo "[ ORPHANED TMP FILES ]"

TMP_ZIPS=$(find /tmp -name "*.zip" -mmin +60 2>/dev/null)

if [ -z "$TMP_ZIPS" ]; then
  echo -e "${GREEN}✓${NC} No orphaned zip files in /tmp"
else
  echo -e "${YELLOW}!${NC} Orphaned zips found:"
  echo "$TMP_ZIPS"
  read -p "Delete them? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    find /tmp -name "*.zip" -mmin +60 -delete
    echo -e "${GREEN}✓${NC} Cleaned up"
  else
    echo "Skipped."
  fi
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Done."


echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  HOSTO — CLEANUP"
echo "  $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# ── 1. Hard delete destroyed sites ───────────────────────────
echo ""
echo "[ DESTROYED SITES ]"

DESTROYED=$($MYSQL "SELECT site FROM sites WHERE status='DESTROYED';")

if [ -z "$DESTROYED" ]; then
  echo -e "${GREEN}✓${NC} No destroyed sites to clean up"
else
  echo "Destroyed sites to remove:"
  for site in $DESTROYED; do
    CUSTOM=$($MYSQL "SELECT COALESCE(custom_domain,'') FROM sites WHERE site='$site';")
    echo "  - $site $([ -n "$CUSTOM" ] && echo "(custom: $CUSTOM)")"
  done
  echo ""
  read -p "Delete all destroyed sites and their jobs? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    for site in $DESTROYED; do
      CUSTOM=$($MYSQL "SELECT COALESCE(custom_domain,'') FROM sites WHERE site='$site';")

      mysql --defaults-file=/root/.my-hosto.cnf controlplane -e "
        DELETE FROM jobs WHERE site='$site';
        DELETE FROM sites WHERE site='$site';
      "
      echo -e "${GREEN}✓${NC} Deleted $site from DB"

      if [ -n "$CUSTOM" ]; then
        sed -i "/hostname: $CUSTOM/,+1d" /etc/cloudflared/config.yml
        echo -e "${GREEN}✓${NC} Removed $CUSTOM from tunnel config"
        RESTART_CLOUDFLARED=1
      fi
    done

    if [ -n "$RESTART_CLOUDFLARED" ]; then
      systemctl restart cloudflared
      echo -e "${GREEN}✓${NC} Cloudflared restarted"
    fi
  else
    echo "Skipped."
  fi
fi

# ── 2. Orphaned containers ────────────────────────────────────
echo ""
echo "[ ORPHANED CONTAINERS ]"

ALL_CONTAINERS=$($DOCKER ps --format '{{.Names}}' | grep -E '^(php_|static_)')
ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status='ACTIVE';")

ORPHANED=""
for container in $ALL_CONTAINERS; do
  site=${container#php_}
  site=${site#static_}
  if ! echo "$ACTIVE_SITES" | grep -qw "$site"; then
    ORPHANED="$ORPHANED $container"
  fi
done

if [ -z "$ORPHANED" ]; then
  echo -e "${GREEN}✓${NC} No orphaned containers"
else
  echo -e "${YELLOW}!${NC} Orphaned containers found:$ORPHANED"
  read -p "Remove them? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    for container in $ORPHANED; do
      $DOCKER stop "$container" 2>/dev/null
      $DOCKER rm "$container" 2>/dev/null
      echo -e "${GREEN}✓${NC} Removed $container"
    done
  else
    echo "Skipped."
  fi
fi

# ── 3. Orphaned nginx configs ─────────────────────────────────
echo ""
echo "[ ORPHANED NGINX CONFIGS ]"

ALL_CONFIGS=$($DOCKER exec edge-nginx ls /etc/nginx/conf.d/ | grep -v '000-default.conf')
ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status='ACTIVE';")

ORPHANED_CONF=""
for conf in $ALL_CONFIGS; do
  site=${conf%.conf}
  if ! echo "$ACTIVE_SITES" | grep -qw "$site"; then
    ORPHANED_CONF="$ORPHANED_CONF $conf"
  fi
done

if [ -z "$ORPHANED_CONF" ]; then
  echo -e "${GREEN}✓${NC} No orphaned nginx configs"
else
  echo -e "${YELLOW}!${NC} Orphaned configs found:$ORPHANED_CONF"
  read -p "Remove them? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    for conf in $ORPHANED_CONF; do
      $DOCKER exec edge-nginx rm -f "/etc/nginx/conf.d/$conf"
      echo -e "${GREEN}✓${NC} Removed $conf"
    done
    $DOCKER exec edge-nginx nginx -s reload
    echo -e "${GREEN}✓${NC} Nginx reloaded"
  else
    echo "Skipped."
  fi
fi

# ── 4. Orphaned tunnel config entries ─────────────────────────
echo ""
echo "[ TUNNEL CONFIG ]"

TUNNEL_DOMAINS=$(grep "hostname:" /etc/cloudflared/config.yml | \
  grep -v "cowsaidmoo.tech" | \
  grep -v "api\." | \
  awk '{print $3}' | tr -d "'")

if [ -z "$TUNNEL_DOMAINS" ]; then
  echo -e "${GREEN}✓${NC} No custom domains in tunnel config"
else
  for domain in $TUNNEL_DOMAINS; do
    EXISTS=$($MYSQL "SELECT COUNT(*) FROM sites WHERE custom_domain='$domain' AND status='ACTIVE';")
    if [ "$EXISTS" = "0" ]; then
      echo -e "${YELLOW}!${NC} $domain in tunnel but no active site"
      read -p "Remove $domain from tunnel config? [y/N] " confirm
      if [ "$confirm" = "y" ]; then
        sed -i "/hostname: $domain/,+1d" /etc/cloudflared/config.yml
        echo -e "${GREEN}✓${NC} Removed $domain"
        RESTART_CLOUDFLARED=1
      fi
    else
      echo -e "${GREEN}✓${NC} $domain active"
    fi
  done

  if [ -n "$RESTART_CLOUDFLARED" ]; then
    systemctl restart cloudflared
    echo -e "${GREEN}✓${NC} Cloudflared restarted"
  fi
fi

# ── 5. Old failed jobs ────────────────────────────────────────
echo ""
echo "[ OLD FAILED JOBS ]"

OLD_FAILED=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='FAILED' AND updated_at < NOW() - INTERVAL 7 DAY;")

if [ "$OLD_FAILED" = "0" ]; then
  echo -e "${GREEN}✓${NC} No old failed jobs"
else
  echo -e "${YELLOW}!${NC} $OLD_FAILED failed job(s) older than 7 days"
  read -p "Delete them? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    mysql --defaults-file=/root/.my-hosto.cnf controlplane -e \
      "DELETE FROM jobs WHERE status='FAILED' AND updated_at < NOW() - INTERVAL 7 DAY;"
    echo -e "${GREEN}✓${NC} Deleted $OLD_FAILED old failed jobs"
  else
    echo "Skipped."
  fi
fi

# ── 6. Orphaned tmp zips ──────────────────────────────────────
echo ""
echo "[ ORPHANED TMP FILES ]"

TMP_ZIPS=$(find /tmp -name "*.zip" -mmin +60 2>/dev/null)

if [ -z "$TMP_ZIPS" ]; then
  echo -e "${GREEN}✓${NC} No orphaned zip files in /tmp"
else
  echo -e "${YELLOW}!${NC} Orphaned zips found:"
  echo "$TMP_ZIPS"
  read -p "Delete them? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    find /tmp -name "*.zip" -mmin +60 -delete
    echo -e "${GREEN}✓${NC} Cleaned up"
  else
    echo "Skipped."
  fi
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Done."
