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

pass() { echo -e "${GREEN}✓${NC} $1"; }
fail() { echo -e "${RED}✗${NC} $1"; CHECKS_FAILED=1; }
warn() { echo -e "${YELLOW}!${NC} $1"; }

CHECKS_FAILED=0

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  HOSTPLANE — HEALTH CHECK"
echo "  $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# ── 1. Control plane service ──────────────────────────────────
echo ""
echo "[ SERVICES ]"

if systemctl is-active --quiet control-plane; then
  pass "control-plane service running"
else
  fail "control-plane service DOWN"
fi

API_KEY=$(grep '^API_KEY' /opt/control/.env | cut -d= -f2-)
HEALTH_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
  -H "X-API-Key: $API_KEY" \
  http://localhost:8080/api/health)
if [ "$HEALTH_CODE" = "200" ]; then
  pass "API /api/health → 200"
else
  fail "API /api/health → $HEALTH_CODE"
fi

# ── 2. MySQL (state-01) ───────────────────────────────────────
echo ""
echo "[ MYSQL — state-01 ]"

if $MYSQL "SELECT 1;" > /dev/null 2>&1; then
  pass "MySQL connection to state-01"
else
  fail "Cannot connect to MySQL on state-01"
  echo ""
  echo "Cannot reach MySQL — skipping job/site checks."
  exit 1
fi

STUCK=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='PROCESSING' AND started_at < NOW() - INTERVAL 15 MINUTE;")
if [ "$STUCK" = "0" ]; then
  pass "No stuck jobs (PROCESSING >15 min)"
else
  fail "$STUCK job(s) stuck in PROCESSING >15 min"
fi

FAILED_JOBS=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='FAILED';")
if [ "$FAILED_JOBS" = "0" ]; then
  pass "No failed jobs"
else
  warn "$FAILED_JOBS failed job(s) — run cleanup.sh to review"
fi

PENDING=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='PENDING';")
if [ "$PENDING" = "0" ]; then
  pass "No pending jobs"
else
  warn "$PENDING job(s) pending"
fi

# ── 3. Docker connectivity ─────────────────────────────────────
echo ""
echo "[ DOCKER — app-01 ]"

if ! $DOCKER info > /dev/null 2>&1; then
  fail "Cannot connect to Docker on app-01 (TLS)"
  echo ""
  echo "Cannot reach app-01 — skipping container/Caddy checks."
  exit 1
fi
pass "Docker TLS connection to app-01"

# ── 4. Caddy ──────────────────────────────────────────────────
echo ""
echo "[ CADDY ]"

CADDY_STATUS=$($DOCKER inspect caddy --format '{{.State.Status}}' 2>/dev/null)
if [ "$CADDY_STATUS" = "running" ]; then
  pass "caddy running"
else
  fail "caddy NOT running (status: ${CADDY_STATUS:-not found})"
fi

# Verify per-site snippets directory exists inside the container
if $DOCKER exec caddy test -d /etc/caddy/sites 2>/dev/null; then
  pass "/etc/caddy/sites/ directory present in caddy container"
else
  fail "/etc/caddy/sites/ MISSING in caddy container — site snippets cannot be written"
fi

# ── 5. Container state cross-check ───────────────────────────
# Checks every known site (ACTIVE or PROVISIONING) against live Docker state.
# Also reports running containers that have no matching DB entry (orphans).
echo ""
echo "[ CONTAINERS ON APP-01 ]"

KNOWN_SITES=$($MYSQL "SELECT site FROM sites WHERE status IN ('ACTIVE','PROVISIONING');" 2>/dev/null)

for site in $KNOWN_SITES; do
  [ -z "$site" ] && continue
  DB_STATUS=$($MYSQL "SELECT status FROM sites WHERE site='$site';" 2>/dev/null)
  PHP_STATUS=$($DOCKER inspect "php_${site}" --format '{{.State.Status}}' 2>/dev/null)
  NGINX_STATUS=$($DOCKER inspect "nginx_${site}" --format '{{.State.Status}}' 2>/dev/null)

  if [ "$DB_STATUS" = "ACTIVE" ]; then
    # ACTIVE: both containers MUST be running
    if [ "$PHP_STATUS" = "running" ]; then
      pass "php_${site} running  (db: ACTIVE)"
    else
      fail "php_${site} NOT running (${PHP_STATUS:-missing})  (db: ACTIVE)"
    fi
    if [ "$NGINX_STATUS" = "running" ]; then
      pass "nginx_${site} running  (db: ACTIVE)"
    else
      fail "nginx_${site} NOT running (${NGINX_STATUS:-missing})  (db: ACTIVE)"
    fi
  else
    # PROVISIONING: containers may not exist yet
    if [ "$PHP_STATUS" = "running" ] && [ "$NGINX_STATUS" = "running" ]; then
      warn "php_${site} + nginx_${site} both running but still PROVISIONING"
    elif [ "$PHP_STATUS" = "running" ] && [ "$NGINX_STATUS" != "running" ]; then
      warn "php_${site} running, nginx_${site} ${NGINX_STATUS:-missing}  (PROVISIONING — mid-flight or stuck)"
    elif [ -z "$PHP_STATUS" ] && [ -z "$NGINX_STATUS" ]; then
      warn "${site}: no containers yet  (PROVISIONING)"
    else
      warn "${site}: php=${PHP_STATUS:-missing} nginx=${NGINX_STATUS:-missing}  (PROVISIONING)"
    fi
  fi
done

# Orphaned: containers running with no matching DB entry
ALL_RUNNING=$($DOCKER ps --format '{{.Names}}' | grep -E '^(php_|nginx_)' | sort)
while IFS= read -r cname; do
  [ -z "$cname" ] && continue
  site="${cname#php_}"; site="${site#nginx_}"
  if ! echo "$KNOWN_SITES" | grep -qw "$site"; then
    warn "$cname running but site '$site' not in DB (orphan)"
  fi
done <<< "$ALL_RUNNING"

# ── 6. Per-site detailed checks (ACTIVE sites) ───────────────
echo ""
echo "[ ACTIVE SITES ]"

ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status='ACTIVE';")

if [ -z "$ACTIVE_SITES" ]; then
  echo "  No active sites."
else
  while IFS= read -r site; do
    [ -z "$site" ] && continue
    echo ""
    echo "  ── $site ──────────────────────────────"

    # php_<site>
    PHP_STATUS=$($DOCKER inspect "php_${site}" --format '{{.State.Status}}' 2>/dev/null)
    if [ "$PHP_STATUS" = "running" ]; then
      pass "  php_${site} running"
    else
      fail "  php_${site} NOT running (${PHP_STATUS:-not found})"
    fi

    # nginx_<site>
    NGINX_STATUS=$($DOCKER inspect "nginx_${site}" --format '{{.State.Status}}' 2>/dev/null)
    if [ "$NGINX_STATUS" = "running" ]; then
      pass "  nginx_${site} running"
    else
      fail "  nginx_${site} NOT running (${NGINX_STATUS:-not found})"
    fi

    # wp_<site> volume
    if $DOCKER volume inspect "wp_${site}" > /dev/null 2>&1; then
      pass "  volume wp_${site} exists"
    else
      fail "  volume wp_${site} MISSING"
    fi

    # Caddy snippet
    if $DOCKER exec caddy test -f "/etc/caddy/sites/${site}.caddy" 2>/dev/null; then
      pass "  /etc/caddy/sites/${site}.caddy present"
    else
      fail "  /etc/caddy/sites/${site}.caddy MISSING — site will not be proxied"
    fi

    # HTTP smoke test through the actual stack
    DOMAIN="$($MYSQL "SELECT COALESCE(custom_domain, CONCAT(site, '.cowsaidmoo.tech')) FROM sites WHERE site='$site';")"
    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "https://${DOMAIN}" 2>/dev/null)
    if [[ "$HTTP_CODE" =~ ^[23] ]]; then
      pass "  https://${DOMAIN} → $HTTP_CODE"
    elif [ "$HTTP_CODE" = "000" ]; then
      warn "  https://${DOMAIN} → no response (DNS or network issue)"
    else
      warn "  https://${DOMAIN} → $HTTP_CODE (may still be warming up)"
    fi

  done <<< "$ACTIVE_SITES"
fi

# ── 6. Summary ────────────────────────────────────────────────
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
TOTAL_SITES=$($MYSQL "SELECT COUNT(*) FROM sites WHERE status='ACTIVE';")
echo "  Active sites : $TOTAL_SITES"
echo "  Failed jobs  : $FAILED_JOBS"
echo "  Pending jobs : $PENDING"
echo ""
if [ $CHECKS_FAILED -eq 0 ]; then
  echo -e "${GREEN}All checks passed.${NC}"
else
  echo -e "${RED}${CHECKS_FAILED} check(s) FAILED — review above.${NC}"
  exit 1
fi
