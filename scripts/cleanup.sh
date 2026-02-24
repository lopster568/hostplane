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
    CUSTOM_DOMAIN=$($MYSQL "SELECT COALESCE(custom_domain,'') FROM sites WHERE site='$site';")
    DOMAIN_LABEL=""
    [ -n "$CUSTOM_DOMAIN" ] && DOMAIN_LABEL=" | custom_domain: $CUSTOM_DOMAIN"
    echo "  - $site (site: $STATUS, job: $JOB_STATUS$DOMAIN_LABEL)"
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

# ── 2. Jobs stuck in PENDING too long ────────────────────────
# These are jobs the worker has never claimed. Typically left over when the
# control-plane was restarted mid-queue, or jobs that were manually re-inserted.
# Threshold: 30 minutes. The worker polls every 3s, so 30 min = definitely stuck.
echo ""
echo "[ STUCK PENDING JOBS ]"

STUCK_PENDING=$($MYSQL "SELECT j.id, j.site, j.type, j.attempts, j.max_attempts,
    TIMESTAMPDIFF(MINUTE, j.created_at, NOW()) AS age_min
  FROM jobs j
  WHERE j.status='PENDING'
    AND j.created_at < NOW() - INTERVAL 30 MINUTE
  ORDER BY j.created_at;" 2>/dev/null)

if [ -z "$STUCK_PENDING" ]; then
  echo -e "${GREEN}✓${NC} No jobs stuck in PENDING >30 min"
else
  echo -e "${YELLOW}!${NC} Jobs stuck in PENDING >30 min:"
  echo "  ID | SITE | TYPE | ATTEMPTS | AGE(min)"
  while IFS=$'\t' read -r jid site jtype attempts maxatt age; do
    echo "  $jid | $site | $jtype | $attempts/$maxatt | ${age}m"
  done <<< "$STUCK_PENDING"
  echo ""
  echo "Options:"
  echo "  r) Reset them to PENDING with attempt count zeroed (worker will re-try)"
  echo "  d) Delete them (remove from queue permanently)"
  echo "  s) Skip"
  read -p "Choice [r/d/s]: " choice
  case "$choice" in
    r)
      while IFS=$'\t' read -r jid site _rest; do
        mysql --defaults-file=/root/.my-hosto.cnf controlplane -e "
          UPDATE jobs SET status='PENDING', attempts=0, started_at=NULL,
            updated_at=NOW() WHERE id='$jid';"
        echo -e "${GREEN}✓${NC} Reset job $jid (site: $site)"
      done <<< "$STUCK_PENDING"
      ;;
    d)
      while IFS=$'\t' read -r jid site _rest; do
        mysql --defaults-file=/root/.my-hosto.cnf controlplane -e "
          DELETE FROM jobs WHERE id='$jid';
          UPDATE sites SET status='FAILED', updated_at=NOW() WHERE job_id='$jid';"
        echo -e "${GREEN}✓${NC} Deleted job $jid, marked site '$site' FAILED"
      done <<< "$STUCK_PENDING"
      ;;
    *)
      echo "Skipped."
      ;;
  esac
fi

# Also catch sites whose status is still PROVISIONING but the worker has given
# up on the job (all retries exhausted → job=FAILED, site still=PROVISIONING).
# These are zombie sites: no containers, no DB entry, but occupy a site name.
echo ""
echo "[ PROVISIONING ZOMBIES ]"

ZOMBIES=$($MYSQL "SELECT s.site,
    COALESCE(CAST(j.attempts AS CHAR), '?') AS attempts,
    COALESCE(CAST(j.max_attempts AS CHAR), '?') AS max_att,
    COALESCE(CAST(TIMESTAMPDIFF(MINUTE, j.updated_at, NOW()) AS CHAR),
             CAST(TIMESTAMPDIFF(MINUTE, s.updated_at, NOW()) AS CHAR)) AS idle_min
  FROM sites s
  LEFT JOIN jobs j ON s.job_id = j.id
  WHERE s.status = 'PROVISIONING'
    AND (j.status = 'FAILED' OR j.id IS NULL)
  ORDER BY s.updated_at;" 2>/dev/null)

if [ -z "$ZOMBIES" ]; then
  echo -e "${GREEN}✓${NC} No zombie PROVISIONING sites"
else
  echo -e "${YELLOW}!${NC} Sites stuck in PROVISIONING with all job retries exhausted:"
  while IFS=$'\t' read -r site attempts maxatt idle; do
    echo "  - $site (attempts: $attempts/$maxatt, idle: ${idle}m)"
  done <<< "$ZOMBIES"
  echo ""
  read -p "Mark them FAILED and remove from queue? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    while IFS=$'\t' read -r site _rest; do
      mysql --defaults-file=/root/.my-hosto.cnf controlplane -e "
        UPDATE sites SET status='FAILED', updated_at=NOW() WHERE site='$site';
        DELETE FROM jobs WHERE site='$site' AND status='FAILED';"
      echo -e "${GREEN}✓${NC} Marked $site as FAILED, cleared job"
    done <<< "$ZOMBIES"
  else
    echo "Skipped."
  fi
fi

# ── 3. Orphaned containers ─────────────────────────────────────
echo ""
echo "[ ORPHANED CONTAINERS ]"

# Collect all php_* and nginx_* containers running on app-01
ALL_CONTAINERS=$($DOCKER ps --format '{{.Names}}' | grep -E '^(php_|nginx_)')
ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status IN ('ACTIVE','DOMAIN_ACTIVE');")

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

# ── 4. Orphaned Docker volumes ────────────────────────────────
echo ""
echo "[ ORPHANED VOLUMES ]"

ALL_VOLUMES=$($DOCKER volume ls --format '{{.Name}}' | grep '^wp_')
ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status IN ('ACTIVE','DOMAIN_ACTIVE');")

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

# ── 5. Orphaned Caddy snippets ─────────────────────────────────
echo ""
echo "[ ORPHANED CADDY CONFIGS ]"

ALL_SNIPPETS=$($DOCKER exec caddy sh -c 'ls /etc/caddy/sites/*.caddy 2>/dev/null' \
  | xargs -n1 basename 2>/dev/null | sed 's/\.caddy$//')
ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status IN ('ACTIVE','DOMAIN_ACTIVE');")

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

# ── 6. Stale custom domain DB records ─────────────────────────
# Sites that are DESTROYED or FAILED with custom_domain still set in the
# controlplane DB. The Caddy snippet is already gone (handled above) but
# the custom_domain column should be cleared so the name can be reused.
echo ""
echo "[ STALE CUSTOM DOMAINS ]"

STALE_DOMAINS=$($MYSQL "SELECT site, custom_domain FROM sites \
  WHERE custom_domain IS NOT NULL AND custom_domain != '' \
  AND status IN ('DESTROYED','FAILED') \
  ORDER BY site;" 2>/dev/null)

if [ -z "$STALE_DOMAINS" ]; then
  echo -e "${GREEN}✓${NC} No stale custom domain records"
else
  echo -e "${YELLOW}!${NC} Sites with custom_domain still set but status DESTROYED/FAILED:"
  while IFS=$'\t' read -r site domain; do
    STATUS=$($MYSQL "SELECT status FROM sites WHERE site='$site';")
    echo "  - $site → $domain (status: $STATUS)"
  done <<< "$STALE_DOMAINS"
  echo ""
  read -p "Clear custom_domain field for these sites? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    while IFS=$'\t' read -r site domain; do
      mysql --defaults-file=/root/.my-hosto.cnf controlplane -e \
        "UPDATE sites SET custom_domain=NULL, updated_at=NOW() WHERE site='$site';"
      echo -e "${GREEN}✓${NC} Cleared custom_domain for $site (was: $domain)"
    done <<< "$STALE_DOMAINS"
  else
    echo "Skipped."
  fi
fi

# Also detect ACTIVE/DOMAIN_ACTIVE sites whose custom_domain is set in DB but
# the containers are gone — these are externally destroyed without going through
# the API. Clear the custom_domain and mark them FAILED so cleanup section 1
# can pick them up on next run.
echo ""
echo "[ CUSTOM DOMAIN — CONTAINER MISMATCH ]"

DOMAIN_ACTIVE_SITES=$($MYSQL "SELECT site, custom_domain FROM sites \
  WHERE custom_domain IS NOT NULL AND custom_domain != '' \
  AND status IN ('ACTIVE','DOMAIN_ACTIVE');" 2>/dev/null)

MISMATCH_FOUND=0
while IFS=$'\t' read -r site domain; do
  [ -z "$site" ] && continue
  PHP_STATUS=$($DOCKER inspect "php_${site}" --format '{{.State.Status}}' 2>/dev/null)
  if [ -z "$PHP_STATUS" ]; then
    echo -e "${YELLOW}!${NC} $site has custom_domain=$domain but php_${site} is gone"
    MISMATCH_FOUND=1
  fi
done <<< "$DOMAIN_ACTIVE_SITES"

if [ "$MISMATCH_FOUND" = "0" ]; then
  echo -e "${GREEN}✓${NC} All sites with custom domains have running containers"
else
  echo ""
  read -p "Remove Caddy snippet + clear custom_domain for missing-container sites? [y/N] " confirm
  if [ "$confirm" = "y" ]; then
    while IFS=$'\t' read -r site domain; do
      [ -z "$site" ] && continue
      PHP_STATUS=$($DOCKER inspect "php_${site}" --format '{{.State.Status}}' 2>/dev/null)
      if [ -z "$PHP_STATUS" ]; then
        # Remove Caddy snippet for this site
        $DOCKER exec caddy rm -f "/etc/caddy/sites/${site}.caddy" 2>/dev/null
        $DOCKER exec caddy caddy reload --config /etc/caddy/Caddyfile 2>/dev/null
        # Clear custom_domain and mark FAILED in DB
        mysql --defaults-file=/root/.my-hosto.cnf controlplane -e \
          "UPDATE sites SET custom_domain=NULL, status='FAILED', updated_at=NOW() WHERE site='$site';"
        echo -e "${GREEN}✓${NC} Cleaned up $site — Caddy snippet removed, custom_domain cleared, marked FAILED"
      fi
    done <<< "$DOMAIN_ACTIVE_SITES"
  else
    echo "Skipped."
  fi
fi

# ── 7. Old failed jobs ─────────────────────────────────────────
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

# ── 8. Orphaned tmp files ──────────────────────────────────────
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
