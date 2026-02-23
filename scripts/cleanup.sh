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
    echo "  - $site"
  done
  echo ""
  read -p "Delete all destroyed sites and their jobs? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    for site in $DESTROYED; do
      mysql -u control -p'control@123' -h 10.10.0.20 controlplane -e \
        "DELETE FROM jobs WHERE site='$site'; DELETE FROM sites WHERE site='$site';"
      echo -e "${GREEN}✓${NC} Deleted $site"
    done
  else
    echo "Skipped."
  fi
fi

echo ""

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
