#!/bin/bash

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

MYSQL="mysql --defaults-file=/root/.my-hosto.cnf -se"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  HOSTO — CLEANUP"
echo "  $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# 1 — Hard delete destroyed sites and their jobs
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
echo ""

echo ""
echo "[ TUNNEL CONFIG ]"

# Find domains in cloudflared config that no longer have active sites
TUNNEL_DOMAINS=$(grep "hostname:" /etc/cloudflared/config.yml | \
  grep -v "cowsaidmoo.tech" | \
  awk '{print $3}')

if [ -z "$TUNNEL_DOMAINS" ]; then
  echo -e "${GREEN}✓${NC} No custom domains in tunnel config"
else
  for domain in $TUNNEL_DOMAINS; do
    EXISTS=$($MYSQL "SELECT COUNT(*) FROM sites WHERE custom_domain='$domain' AND status='ACTIVE';")
    if [ "$EXISTS" = "0" ]; then
      echo -e "${YELLOW}!${NC} $domain in tunnel config but no active site"
      read -p "Remove $domain from tunnel config? [y/N] " confirm
      if [ "$confirm" = "y" ]; then
        # Remove from config.yml
        sed -i "/hostname: $domain/,+1d" /etc/cloudflared/config.yml
        echo -e "${GREEN}✓${NC} Removed $domain from tunnel config"
        systemctl restart cloudflared
        echo -e "${GREEN}✓${NC} Cloudflared restarted"
      fi
    else
      echo -e "${GREEN}✓${NC} $domain active"
    fi
  done
fi

# 2 — Clear failed jobs older than 7 days
OLD_FAILED=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='FAILED' AND updated_at < NOW() - INTERVAL 7 DAY;")
if [ "$OLD_FAILED" = "0" ]; then
  echo -e "${GREEN}✓${NC} No old failed jobs"
else
  echo -e "${YELLOW}!${NC} $OLD_FAILED failed jobs older than 7 days"
  read -p "Delete them? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    mysql -u control -p'control@123' -h 10.10.0.20 controlplane -e \
      "DELETE FROM jobs WHERE status='FAILED' AND updated_at < NOW() - INTERVAL 7 DAY;"
    echo -e "${GREEN}✓${NC} Deleted $OLD_FAILED old failed jobs"
  fi
fi

echo ""

# 3 — Clean up orphaned tmp zips
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
  fi
fi

echo ""
echo "Done."
