# Caddy Cert Status & Routing Fixes

## Problem 1 — `caddy list-certificates` Does Not Exist

### Symptom

`cert_status` was always `"pending"` even for sites that were working fine in
the browser.

### Root Cause

`caddyHasCert` ran `caddy list-certificates` inside the Caddy container.
This command was removed from newer Caddy versions:

```
Error: unknown command "list-certificates" for "caddy"
```

The exec returned exit code 1 with no output, so the function always returned
`false` regardless of whether the cert was on disk.

### Fix

Replaced with a `test -f` exec against Caddy's on-disk ACME cert storage:

```
/data/caddy/certificates/acme-v02.api.letsencrypt.org-directory/<domain>/<domain>.crt
```

The exit code of `test -f` is `0` if the file exists, `1` if not — reliable
across all Caddy versions.

---

## Problem 2 — `regenerateCaddy` Never Reloaded Caddy

### Symptom

After setting or removing a custom domain, Caddy's running config remained
stale. The snippet on disk was correct, but Caddy kept routing the old way
until a manual `caddy reload` was run.

Concretely: `phoenixdns.app` was moved from `testsite01` to `testsite02`.
The `testsite02.caddy` snippet was written correctly, but Caddy's running
config still held the old state → `ERR_SSL_PROTOCOL_ERROR` in the browser
because Caddy had no active TLS policy for `phoenixdns.app` under the new
site.

### Root Cause

`regenerateCaddy` only wrote the snippet file. It never called `reloadCaddy`.
Both `handleSetCustomDomain` and `handleRemoveCustomDomain` relied on
`regenerateCaddy` and therefore never triggered a reload.

### Fix

`regenerateCaddy` now calls `reloadCaddy` after writing the snippet.
Every domain add/remove is immediately reflected in Caddy's running config
with zero manual intervention.

---

## Problem 3 — `GET /api/sites/:site` Was Blind to Infra State

### Symptom

The API returned `"cert_status": "pending"` or `"issued"` but gave no
indication of whether Caddy was actually routing the domain. A broken site
(missing snippet, stale snippet, cert not yet issued) was indistinguishable
from a healthy one just by looking at the response.

### Fix

`GET /api/sites/:site` now runs three live infra checks and returns a
`warnings` array with actionable messages:

| Check                                  | Warning when failing                                                    |
| -------------------------------------- | ----------------------------------------------------------------------- |
| Cert file on disk                      | `"TLS cert not yet issued — call /cert-retry"`                          |
| Snippet file exists in Caddy container | `"Caddy config snippet missing — site will not be routed"`              |
| Snippet contains the active domain     | `"Caddy snippet exists but does not route <domain> — call /cert-retry"` |

Example healthy response:

```json
{
  "site": "testsite02",
  "domain": "testsite02.cowsaidmoo.tech",
  "custom_domain": "phoenixdns.app",
  "status": "ACTIVE",
  "cert_status": "issued",
  "warnings": []
}
```

Example broken response (would have caught today's issue automatically):

```json
{
  "cert_status": "pending",
  "warnings": [
    "Caddy snippet exists but does not route phoenixdns.app — reload may be needed; call /cert-retry"
  ]
}
```

---

## New Endpoint — `POST /api/sites/:site/cert-retry`

Forces a Caddy reload to re-queue ACME issuance, then polls 30 s and returns
the current `cert_status`. Use when a domain has been pending > ~10 minutes
or after fixing a DNS/network issue.

```bash
curl -X POST https://<control-plane>/api/sites/testsite02/cert-retry \
  -H "X-API-Key: ..."
# {"site":"testsite02","domain":"phoenixdns.app","cert_status":"issued"}
```
