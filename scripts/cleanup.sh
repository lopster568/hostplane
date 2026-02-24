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
echo "  HOSTPLANE — CLEANUP"
echo "  $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# ── 1. Destroyed / failed sites ───────────────────────────────
echo ""
echo "[ DESTROYED / FAILED SITES ]"

DEAD_SITES=$($MYSQL "SELECT site FROM sites WHERE status IN ('DESTROYED','FAILED');")

# Also catch PROVISIONING sites whose job has exhausted all retries (stuck ghosts)
STRANDED=$($MYSQL "SELECT s.site FROM sites s JOIN jobs j ON s.job_id = j.id \
  WHERE s.status='PROVISIONING' AND j.status='FAILED';")

ALL_DEAD=$(echo -e "$DEAD_SITES\n$STRANDED" | sort -u | grep -v '^$')

if [ -z "$ALL_DEAD" ]; then
  echo -e "${GREEN}✓${NC} No destroyed/failed/stranded sites to clean up"
else
  echo "Sites to remove:"
  for site in $ALL_DEAD; do
    STATUS=$($MYSQL "SELECT status FROM sites WHERE site='$site';")
    JOB_STATUS=$($MYSQL "SELECT COALESCE(j.status,'?') FROM sites s LEFT JOIN jobs j ON s.job_id=j.id WHERE s.site='$site';")
    echo "  - $site (site: $STATUS, job: $JOB_STATUS)"
  done
  echo ""
  read -p "Delete all dead/stranded sites and their jobs from DB? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    for site in $ALL_DEAD; do
      mysql --defaults-file=/root/.my-hosto.cnf controlplane -e "
        DELETE FROM jobs WHERE site='$site';
        DELETE FROM sites WHERE site='$site';
      "
      echo -e "${GREEN}✓${NC} Deleted $site from DB"
    done
  else
    echo "Skipped."
  fi
fi

# ── 2. Orphaned containers ─────────────────────────────────────
echo ""
echo "[ ORPHANED CONTAINERS ]"

# Collect all php_* and nginx_* containers running on app-01
ALL_CONTAINERS=$($DOCKER ps --format '{{.Names}}' | grep -E '^(php_|nginx_)')
ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status='ACTIVE';")

ORPHANED=""
for cname in $ALL_CONTAINERS; do
  # Strip prefix to get site name
  site="${cname#php_}"
  site="${site#nginx_}"
  if ! echo "$ACTIVE_SITES" | grep -qw "$site"; then
    ORPHANED="$ORPHANED $cname"
  fi
done

if [ -z "$ORPHANED" ]; then
  echo -e "${GREEN}✓${NC} No orphaned containers"
else
  echo -e "${YELLOW}!${NC} Orphaned containers:$ORPHANED"
  read -p "Remove them? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    for cname in $ORPHANED; do
      $DOCKER rm -f "$cname" 2>/dev/null
      echo -e "${GREEN}✓${NC} Removed $cname"
    done
  else
    echo "Skipped."
  fi
fi

# ── 3. Orphaned Docker volumes ────────────────────────────────
echo ""
echo "[ ORPHANED VOLUMES ]"

ALL_VOLUMES=$($DOCKER volume ls --format '{{.Name}}' | grep '^wp_')
ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status='ACTIVE';")

ORPHANED_VOLS=""
for vol in $ALL_VOLUMES; do
  site="${vol#wp_}"
  if ! echo "$ACTIVE_SITES" | grep -qw "$site"; then
    ORPHANED_VOLS="$ORPHANED_VOLS $vol"
  fi
done

if [ -z "$ORPHANED_VOLS" ]; then
  echo -e "${GREEN}✓${NC} No orphaned volumes"
else
  echo -e "${YELLOW}!${NC} Orphaned volumes:$ORPHANED_VOLS"
  read -p "Remove them? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    for vol in $ORPHANED_VOLS; do
      $DOCKER volume rm "$vol" 2>/dev/null
      echo -e "${GREEN}✓${NC} Removed $vol"
    done
  else
    echo "Skipped."
  fi
fi

# ── 4. Orphaned Caddy snippets ─────────────────────────────────
echo ""
echo "[ ORPHANED CADDY CONFIGS ]"

ALL_SNIPPETS=$($DOCKER exec caddy sh -c 'ls /etc/caddy/sites/*.caddy 2>/dev/null' \
  | xargs -n1 basename 2>/dev/null | sed 's/\.caddy$//')
ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status='ACTIVE';")

ORPHANED_CADDY=""
for site in $ALL_SNIPPETS; do
  if ! echo "$ACTIVE_SITES" | grep -qw "$site"; then
    ORPHANED_CADDY="$ORPHANED_CADDY $site"
  fi
done

if [ -z "$ORPHANED_CADDY" ]; then
  echo -e "${GREEN}✓${NC} No orphaned Caddy snippets"
else
  echo -e "${YELLOW}!${NC} Orphaned Caddy snippets:$ORPHANED_CADDY"
  read -p "Remove them and reload Caddy? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    for site in $ORPHANED_CADDY; do
      $DOCKER exec caddy rm -f "/etc/caddy/sites/${site}.caddy"
      echo -e "${GREEN}✓${NC} Removed ${site}.caddy"
    done
    $DOCKER exec caddy caddy reload --config /etc/caddy/Caddyfile
    echo -e "${GREEN}✓${NC} Caddy reloaded"
  else
    echo "Skipped."
  fi
fi

# ── 5. Old failed jobs ─────────────────────────────────────────
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

# ── 6. Orphaned tmp files ──────────────────────────────────────
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
