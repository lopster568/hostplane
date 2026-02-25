# TLS Cert Pending — Issue & Fix

## What Happened

After provisioning a site or adding a custom domain, the job completes and returns
`200` immediately — but Caddy's ACME cert issuance is async. Let's Encrypt hasn't
issued the cert yet when the browser first hits the domain, causing:

```
ERR_SSL_PROTOCOL_ERROR
```

Observed on:

- First visit to `phoenixdns.app` after `POST /api/sites/testsite02/domain`
- First visit to newly provisioned `*.cowsaidmoo.tech` subdomains

Root cause: Caddy queues ACME requests with an internal rate limiter. When multiple
domains are provisioned back-to-back, later domains wait behind earlier ones.
The control plane was returning success before any cert was in hand.

---

## Fix Applied

### 1. `caddy.go` — `PollCaddyCert` + `caddyHasCert`

Added two helpers:

- `caddyHasCert(docker, cfg, domain)` — execs `caddy list-certificates` inside the
  Caddy container and returns true if the domain appears.
- `PollCaddyCert(docker, cfg, domain, timeout)` — polls every 3s up to `timeout`,
  returns `"issued"` or `"pending"`.

### 2. `provisioner.go` — Step 8 after reloadCaddy

```go
certStatus := PollCaddyCert(p.docker, p.cfg, domain, 30*time.Second)
log.Printf("[provisioner] site=%s cert_status=%s", site, certStatus)
```

### 3. `static_provisioner.go` — Step 4 after reloadCaddy

Same poll added after the Caddy reload step.

### 4. `api.go` — `handleSetCustomDomain`

After DB commit, polls for cert and returns `cert_status` in the response:

```json
{
  "site": "testsite02",
  "default_domain": "testsite02.cowsaidmoo.tech",
  "custom_domain": "phoenixdns.app",
  "cert_status": "issued",
  "status": "active"
}
```

### 5. `api.go` — `GET /api/sites/:site`

Now returns a live `cert_status` field (`"issued"` or `"pending"`) for
`ACTIVE` / `DOMAIN_ACTIVE` sites by calling `caddyHasCert` on each request.

### 6. `scripts/cleanup.sh` — Section 6: TLS Cert Readiness

New section that loops over all active custom domains, does a real HTTPS connection
check, and reports which are cert-pending with instructions to force a Caddy reload.

### 7. `scripts/health.sh` — Per-site TLS check

Custom domain check now includes a live `curl` to `https://<custom_domain>` and
distinguishes SSL handshake failure (cert not issued) from HTTP errors.

---

## Behaviour After Fix

- Provisioner jobs now log `cert_status=issued` or `cert_status=pending` on completion.
- API responses include `cert_status` so callers know whether to expect immediate HTTPS.
- Cert pending is **not an error** — Caddy retries ACME automatically. A `caddy reload`
  forces an immediate retry if stuck >~10 minutes.
