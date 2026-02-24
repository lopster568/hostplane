#!/bin/bash

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

DOCKER="docker --host tcp://10.10.0.10:2376 --tlsverify \
  --tlscacert /opt/control/certs/ca.pem \
  --tlscert /opt/control/certs/cert.pem \
  --tlskey /opt/control/certs/key.pem"

MYSQL="mysql --defaults-file=/root/.my-hosto.cnf -se"

pass() { echo -e "${GREEN}✓${NC} $1"; }
fail() { echo -e "${RED}✗${NC} $1"; CHECKS_FAILED=1; }
warn() { echo -e "${YELLOW}!${NC} $1"; }

CHECKS_FAILED=0

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  HOSTO CONTROL PLANE — HEALTH CHECK"
echo "  $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

echo ""
echo "[ SERVICES ]"

# Control plane API
if systemctl is-active --quiet control-plane; then
  pass "control-plane service running"
else
  fail "control-plane service DOWN"
fi

# Cloudflared
if systemctl is-active --quiet cloudflared; then
  pass "cloudflared service running"
else
  fail "cloudflared service DOWN"
fi

# API health endpoint
API_KEY=$(grep '^API_KEY' /opt/control/.env | cut -d= -f2-)
HEALTH=$(curl -s -o /dev/null -w "%{http_code}" \
  -H "X-API-Key: $API_KEY" \
  http://localhost:8080/api/health)
if [ "$HEALTH" = "200" ]; then
  pass "API health endpoint responding (200)"
else
  fail "API health endpoint returned $HEALTH"
fi

echo ""
echo "[ MYSQL ]"

if $MYSQL "SELECT 1;" > /dev/null 2>&1; then
  pass "MySQL connection to state-01"
else
  fail "Cannot connect to MySQL on state-01"
fi

# Stuck jobs
STUCK=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='PROCESSING' AND started_at < NOW() - INTERVAL 15 MINUTE;")
if [ "$STUCK" = "0" ]; then
  pass "No stuck jobs"
else
  fail "$STUCK job(s) stuck in PROCESSING >15min"
fi

# Failed jobs
FAILED_JOBS=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='FAILED';")
if [ "$FAILED_JOBS" = "0" ]; then
  pass "No failed jobs"
else
  warn "$FAILED_JOBS failed job(s) — run cleanup script to review"
fi

# Pending jobs
PENDING=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='PENDING';")
if [ "$PENDING" = "0" ]; then
  pass "No pending jobs"
else
  warn "$PENDING job(s) pending"
fi

echo ""
echo "[ DOCKER — app-01 ]"

# Docker connectivity
if $DOCKER info > /dev/null 2>&1; then
  pass "Docker TLS connection to app-01"
else
  fail "Cannot connect to Docker on app-01"
  echo ""
  echo "[ SUMMARY ]"
  echo -e "${RED}Cannot reach app-01 — skipping container/caddy checks.${NC}"
  exit 1
fi

# Caddy
if $DOCKER inspect caddy --format '{{.State.Status}}' 2>/dev/null | grep -q running; then
  pass "caddy running"
else
  fail "caddy NOT running"
fi

# Caddyfile has import line
if $DOCKER exec caddy grep -q 'import /etc/caddy/sites/\*' /etc/caddy/Caddyfile 2>/dev/null; then
  pass "Caddyfile has import /etc/caddy/sites/*"
else
  fail "Caddyfile MISSING 'import /etc/caddy/sites/*' — per-site snippets will not be loaded"
fi

echo ""
echo "[ ACTIVE SITE CONTAINERS ]"

ACTIVE_SITES=$($MYSQL "SELECT site FROM sites WHERE status='ACTIVE';")

if [ -z "$ACTIVE_SITES" ]; then
  echo "  No active sites"
else
  while IFS= read -r site; do
    [ -z "$site" ] && continue

    # Determine container: static sites use static_ prefix, WP sites use php_
    STATIC_STATUS=$($DOCKER inspect "static_${site}" --format '{{.State.Status}}' 2>/dev/null)
    PHP_STATUS=$($DOCKER inspect "php_${site}" --format '{{.State.Status}}' 2>/dev/null)

    if [ -n "$STATIC_STATUS" ]; then
      CONTAINER="static_${site}"
      STATUS="$STATIC_STATUS"
    elif [ -n "$PHP_STATUS" ]; then
      CONTAINER="php_${site}"
      STATUS="$PHP_STATUS"
    else
      fail "$site — no container found (expected php_$site or static_$site)"
      continue
    fi

    if [ "$STATUS" = "running" ]; then
      pass "$CONTAINER running"
    else
      fail "$CONTAINER is '$STATUS' (expected running)"
    fi

    # Check caddy snippet exists
    if $DOCKER exec caddy test -f "/etc/caddy/sites/${site}.caddy" 2>/dev/null; then
      pass "  /etc/caddy/sites/${site}.caddy present"
    else
      fail "  /etc/caddy/sites/${site}.caddy MISSING — site will not be proxied"
    fi
  done <<< "$ACTIVE_SITES"
fi

echo ""
echo "[ CUSTOM DOMAINS ]"

CUSTOM_DOMAINS=$($MYSQL "SELECT site, custom_domain FROM sites WHERE custom_domain IS NOT NULL AND custom_domain != '' AND status='ACTIVE';")

if [ -z "$CUSTOM_DOMAINS" ]; then
  echo "  No custom domains configured"
else
  while IFS=$'\t' read -r site domain; do
    [ -z "$domain" ] && continue
    if grep -q "$domain" /etc/cloudflared/config.yml; then
      pass "$domain → $site (tunnel configured)"
    else
      fail "$domain → $site (MISSING from tunnel config)"
    fi
    RESOLVED=$(dig +short "$domain" 2>/dev/null | head -1)
    if [ -n "$RESOLVED" ]; then
      pass "$domain resolves to $RESOLVED"
    else
      warn "$domain DNS not resolving yet"
    fi
  done <<< "$CUSTOM_DOMAINS"
fi

echo ""
echo "[ SUMMARY ]"
TOTAL_SITES=$($MYSQL "SELECT COUNT(*) FROM sites WHERE status='ACTIVE';")
echo "  Active sites : $TOTAL_SITES"
echo "  Failed jobs  : $FAILED_JOBS"
echo "  Pending jobs : $PENDING"

echo ""
if [ $CHECKS_FAILED -eq 0 ]; then
  echo -e "${GREEN}All checks passed.${NC}"
else
  echo -e "${RED}Some checks FAILED. Review above.${NC}"
  exit 1
fi


echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  HOSTO CONTROL PLANE — HEALTH CHECK"
echo "  $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

echo ""
echo "[ SERVICES ]"

# Control plane API
if systemctl is-active --quiet control-plane; then
  pass "control-plane service running"
else
  fail "control-plane service DOWN"
fi

# Cloudflared
if systemctl is-active --quiet cloudflared; then
  pass "cloudflared service running"
else
  fail "cloudflared service DOWN"
fi

echo ""
echo "[ CUSTOM DOMAINS ]"

CUSTOM_DOMAINS=$($MYSQL "SELECT site, custom_domain FROM sites WHERE custom_domain IS NOT NULL AND status='ACTIVE';")

if [ -z "$CUSTOM_DOMAINS" ]; then
  echo -e "  No custom domains configured"
else
  while IFS=$'\t' read -r site domain; do
    # Check if domain is in cloudflared config
    if grep -q "$domain" /etc/cloudflared/config.yml; then
      pass "$domain → $site (tunnel configured)"
    else
      fail "$domain → $site (MISSING from tunnel config)"
    fi
    # Check DNS resolves
    RESOLVED=$(dig +short "$domain" 2>/dev/null | head -1)
    if [ -n "$RESOLVED" ]; then
      pass "$domain resolves to $RESOLVED"
    else
      warn "$domain DNS not resolving yet"
    fi
  done <<< "$CUSTOM_DOMAINS"
fi

# API responding
HEALTH=$(curl -s -o /dev/null -w "%{http_code}" \
  -H "X-API-Key: $(grep API_KEY /opt/control/.env | grep -v '#' | cut -d= -f2)" \
  http://localhost:8080/api/health)
if [ "$HEALTH" = "200" ]; then
  pass "API health endpoint responding (200)"
else
  fail "API health endpoint returned $HEALTH"
fi

echo ""
echo "[ DOCKER — app-01 ]"

# Docker connectivity
if $DOCKER info > /dev/null 2>&1; then
  pass "Docker TLS connection to app-01"
else
  fail "Cannot connect to Docker on app-01"
fi

# edge-nginx
if $DOCKER inspect edge-nginx --format '{{.State.Status}}' 2>/dev/null | grep -q running; then
  pass "edge-nginx running"
else
  fail "edge-nginx NOT running"
fi

# Active site containers
# Replace the active sites container check section
ACTIVE_SITES=$($MYSQL "SELECT s.site, j.type FROM sites s JOIN jobs j ON s.job_id = j.id WHERE s.status='ACTIVE';")
if [ -z "$ACTIVE_SITES" ]; then
  echo "  No active sites"
else
  while IFS=$'\t' read -r site type; do
    if [ -z "$site" ]; then
      continue
    fi

    if [ "$type" = "STATIC_PROVISION" ]; then
      CONTAINER="static_${site}"
    else
      CONTAINER="php_${site}"
    fi

    STATUS=$($DOCKER inspect "$CONTAINER" --format '{{.State.Status}}')

    if [ "$STATUS" = "running" ]; then
      pass "$CONTAINER running"
    else
      fail "$CONTAINER expected ACTIVE but container is '$STATUS'"
    fi
  done <<< "$ACTIVE_SITES"
fi
# MySQL connectivity
if $MYSQL "SELECT 1;" > /dev/null 2>&1; then
  pass "MySQL connection to state-01"
else
  fail "Cannot connect to MySQL on state-01"
fi

# Stuck jobs
STUCK=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='PROCESSING' AND started_at < NOW() - INTERVAL 15 MINUTE;")
if [ "$STUCK" = "0" ]; then
  pass "No stuck jobs"
else
  fail "$STUCK job(s) stuck in PROCESSING >15min"
fi

# Failed jobs
FAILED_JOBS=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='FAILED';")
if [ "$FAILED_JOBS" = "0" ]; then
  pass "No failed jobs"
else
  warn "$FAILED_JOBS failed job(s) — run cleanup script to review"
fi

# Pending jobs
PENDING=$($MYSQL "SELECT COUNT(*) FROM jobs WHERE status='PENDING';")
if [ "$PENDING" = "0" ]; then
  pass "No pending jobs"
else
  warn "$PENDING job(s) pending"
fi

echo ""
echo "[ SUMMARY ]"
TOTAL_SITES=$($MYSQL "SELECT COUNT(*) FROM sites WHERE status='ACTIVE';")
TOTAL_STATIC=$($MYSQL "SELECT COUNT(*) FROM sites WHERE status='ACTIVE' AND site IN (SELECT site FROM jobs WHERE type='STATIC_PROVISION');")
echo "  Active sites : $TOTAL_SITES"
echo "  Failed jobs  : $FAILED_JOBS"
echo "  Pending jobs : $PENDING"

echo ""
if [ $FAILED -eq 0 ]; then
  echo -e "${GREEN}All checks passed.${NC}"
else
  echo -e "${RED}Some checks FAILED. Review above.${NC}"
  exit 1
fi
