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

# Use -F (fixed string) — the * in *.caddy is a glob, not a regex metacharacter
if $DOCKER exec caddy grep -qF 'import /etc/caddy/sites/*.caddy' /etc/caddy/Caddyfile 2>/dev/null; then
  pass "Caddyfile has import /etc/caddy/sites/*.caddy"
else
  fail "Caddyfile MISSING 'import /etc/caddy/sites/*.caddy' — per-site snippets will NOT be loaded"
fi

# ── 5. Containers actually running on app-01 ─────────────────
# This section reflects live Docker state, independent of the DB. Useful for
# spotting containers left behind after a failed provision/destroy, or missing
# nginx sidecars that were never started.
echo ""
echo "[ CONTAINERS ON APP-01 ]"

ALL_RUNNING=$($DOCKER ps --format '{{.Names}}' | grep -E '^(php_|nginx_)' | sort)
if [ -z "$ALL_RUNNING" ]; then
  echo "  No php_* or nginx_* containers running."
else
  while IFS= read -r cname; do
    [ -z "$cname" ] && continue
    # Derive site and expected pair
    if [[ "$cname" == php_* ]]; then
      site="${cname#php_}"
      pair="nginx_${site}"
    else
      site="${cname#nginx_}"
      pair="php_${site}"
    fi
    DB_STATUS=$($MYSQL "SELECT COALESCE(status,'NOT IN DB') FROM sites WHERE site='$site';" 2>/dev/null)
    PAIR_STATUS=$($DOCKER inspect "$pair" --format '{{.State.Status}}' 2>/dev/null)
    PAIR_INFO="${pair}:${PAIR_STATUS:-MISSING}"
    if [ "$DB_STATUS" = "ACTIVE" ] && [ "$PAIR_STATUS" = "running" ]; then
      pass "$cname running  (pair: $PAIR_INFO  db: $DB_STATUS)"
    elif [ "$DB_STATUS" = "ACTIVE" ] && [ "$PAIR_STATUS" != "running" ]; then
      fail "$cname running but pair $PAIR_INFO — site incomplete"
    else
      warn "$cname running  (pair: $PAIR_INFO  db: $DB_STATUS)"
    fi
  done <<< "$ALL_RUNNING"
fi

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
