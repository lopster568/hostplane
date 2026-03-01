# Hostplane Control Plane — API Reference

## Base URL

```
https://api.cowsaidmoo.tech/api
```

## Authentication

Every endpoint except `GET /api/health` requires the `X-API-Key` header.

```
X-API-Key: <your-api-key>
```

Returns `401 Unauthorized` if the key is missing or wrong:

```json
{ "error": "unauthorized" }
```

---

## Site Name Rules

- Lowercase letters and numbers only (`^[a-z0-9]+$`)
- Used as the subdomain: `<site>.cowsaidmoo.tech`

---

## Endpoints

| Method   | Path                             | Description                               |
| -------- | -------------------------------- | ----------------------------------------- |
| `GET`    | `/api/health`                    | Health check (no auth)                    |
| `POST`   | `/api/provision`                 | Provision a WordPress site                |
| `POST`   | `/api/static/provision`          | Provision a static site                   |
| `POST`   | `/api/destroy`                   | Destroy a site (queues job)               |
| `GET`    | `/api/sites`                     | List all sites                            |
| `GET`    | `/api/sites/:site`               | Get site status + live infra checks       |
| `DELETE` | `/api/sites/:site`               | Hard delete a DESTROYED site record       |
| `POST`   | `/api/sites/:site/domain`        | Set a custom domain                       |
| `DELETE` | `/api/sites/:site/domain`        | Remove the custom domain                  |
| `GET`    | `/api/sites/:site/domain/status` | Live DNS + cert status (poll from UI)     |
| `POST`   | `/api/sites/:site/cert-retry`    | Force Caddy reload + poll cert            |
| `GET`    | `/api/jobs/:id`                  | Get job status                            |
| `DELETE` | `/api/jobs/:id`                  | Hard delete a completed/failed job record |

---

## `GET /api/health`

Health check. No authentication required.

**Response `200`**

```json
{ "status": "ok" }
```

---

## `POST /api/provision`

Queues a WordPress site provisioning job. Creates MariaDB database, Docker
volume, PHP-FPM container, nginx sidecar, and Caddy snippet. Returns
immediately with a job ID — poll `GET /api/jobs/:id` for completion.

**Request**

```json
{ "site": "mysite" }
```

**Response `202`**

```json
{
  "job_id": "63c83c3f-775d-40b5-96ad-567c3f1e10d3",
  "site": "mysite",
  "domain": "mysite.cowsaidmoo.tech",
  "status": "PENDING"
}
```

**Errors**

| Code  | Reason                                                       |
| ----- | ------------------------------------------------------------ |
| `400` | Invalid or missing site name                                 |
| `409` | Site already ACTIVE, or already has a pending/processing job |

---

## `POST /api/static/provision`

Queues a static site provisioning job. Accepts a zip file, extracts it into
the shared `caddy_static_sites` volume, and configures Caddy's `file_server`.
Returns immediately with a job ID.

**Request** — `multipart/form-data`

| Field  | Type   | Required | Description                    |
| ------ | ------ | -------- | ------------------------------ |
| `site` | string | yes      | Site name (`^[a-z0-9]+$`)      |
| `zip`  | file   | yes      | Zip archive of the static site |

**Response `202`**

```json
{
  "job_id": "abc123",
  "site": "mystaticsite",
  "domain": "mystaticsite.cowsaidmoo.tech",
  "status": "PENDING"
}
```

**Errors**

| Code  | Reason                                                       |
| ----- | ------------------------------------------------------------ |
| `400` | Missing site name, invalid name, or missing zip file         |
| `409` | Site already ACTIVE, or already has a pending/processing job |

---

## `POST /api/destroy`

Queues a destroy job. Tears down all Docker containers, volumes, Caddy
snippet, and database for the site. Returns immediately — poll
`GET /api/jobs/:id` for completion.

**Request**

```json
{ "site": "mysite" }
```

**Response `202`**

```json
{
  "job_id": "abc123",
  "site": "mysite",
  "status": "PENDING"
}
```

**Errors**

| Code  | Reason                                                        |
| ----- | ------------------------------------------------------------- |
| `400` | Invalid or missing site name                                  |
| `404` | Site not found                                                |
| `409` | Site is already DESTROYING or DESTROYED, or has an active job |

---

## `GET /api/sites`

Returns all site records.

**Response `200`**

```json
{
  "sites": [
    {
      "site": "mysite",
      "domain": "mysite.cowsaidmoo.tech",
      "custom_domain": "example.com",
      "status": "ACTIVE",
      "job_id": "63c83c3f-...",
      "created_at": "2026-02-25T02:56:20Z",
      "updated_at": "2026-02-25T02:56:52Z"
    }
  ]
}
```

---

## `GET /api/sites/:site`

Returns site status with live infra checks against the Caddy container.
For `ACTIVE` and `DOMAIN_ACTIVE` sites, verifies:

1. TLS cert is present on Caddy's disk
2. Caddy snippet file exists inside the container
3. Snippet contains the active domain (catches stale config after domain moves)

**Response `200`**

```json
{
  "site": "mysite",
  "domain": "mysite.cowsaidmoo.tech",
  "custom_domain": "example.com",
  "status": "ACTIVE",
  "cert_status": "issued",
  "warnings": [],
  "job_id": "63c83c3f-...",
  "created_at": "2026-02-25T02:56:20Z",
  "updated_at": "2026-02-25T02:56:52Z"
}
```

**`cert_status` values**

| Value       | Meaning                                                |
| ----------- | ------------------------------------------------------ |
| `"issued"`  | TLS cert file present in Caddy's ACME storage          |
| `"pending"` | Cert not yet on disk — Caddy is retrying in background |
| `""`        | Site is not ACTIVE/DOMAIN_ACTIVE — not checked         |

**`warnings` examples**

```json
"warnings": [
  "TLS cert not yet issued — Caddy is retrying ACME in background; call /cert-retry to force"
]
```

```json
"warnings": [
  "Caddy config snippet missing — site will not be routed; re-provision or call /cert-retry"
]
```

```json
"warnings": [
  "Caddy snippet exists but does not route example.com — reload may be needed; call /cert-retry"
]
```

An empty array (`[]`) means all infra checks passed.

**Errors**

| Code  | Reason         |
| ----- | -------------- |
| `404` | Site not found |

---

## `DELETE /api/sites/:site`

Hard deletes the site record from the database. The site **must** be in
`DESTROYED` status. Use `POST /api/destroy` first to destroy infrastructure.

**Response `200`**

```json
{ "deleted": "mysite" }
```

**Errors**

| Code  | Reason                          |
| ----- | ------------------------------- |
| `404` | Site not found                  |
| `409` | Site is not in DESTROYED status |

---

## `POST /api/sites/:site/domain`

Sets a custom domain for an existing ACTIVE site. Validates that:

- Domain format is valid and is not a subdomain of the base domain
- DNS A record for the domain resolves to the VPS public IP
- Domain is not already claimed by another site

For WordPress sites, also updates the nginx `server_name` and `wp_options`
`siteurl` / `home` values. Caddy snippet is rewritten and reloaded atomically.
Polls for cert issuance for up to 30 s before returning.

**Request**

```json
{ "domain": "example.com" }
```

**Response `200`**

```json
{
  "site": "mysite",
  "default_domain": "mysite.cowsaidmoo.tech",
  "custom_domain": "example.com",
  "cert_status": "issued",
  "status": "active"
}
```

**Idempotent:** If the same domain is already set, returns `200` immediately
with `"message": "domain already set"`.

**Errors**

| Code  | Reason                                                                        |
| ----- | ----------------------------------------------------------------------------- |
| `400` | Invalid domain format, domain is a base subdomain, or DNS not pointing to VPS |
| `404` | Site not found                                                                |
| `409` | Site not ACTIVE/DOMAIN_ACTIVE, or domain already claimed by another site      |
| `500` | nginx/Caddy config failed (infra rolled back)                                 |

---

## `DELETE /api/sites/:site/domain`

Removes the custom domain from a site. Reverts Caddy snippet to default
subdomain only, reloads Caddy, and for WordPress sites reverts nginx
`server_name` and `wp_options` URLs.

**Response `200`**

```json
{
  "site": "mysite",
  "domain": "mysite.cowsaidmoo.tech",
  "status": "custom domain removed"
}
```

**Errors**

| Code  | Reason                         |
| ----- | ------------------------------ |
| `400` | No custom domain currently set |
| `404` | Site not found                 |
| `500` | nginx/Caddy revert failed      |

---

## `GET /api/sites/:site/domain/status`

Returns a live snapshot of the custom domain's DNS propagation and TLS cert
state. **No infra changes are made** — safe to poll every few seconds from the
UI during the "add domain" setup flow.

Designed to power a setup checklist like:

```
[ ] Point phoenixdns.app → 129.212.247.213   ← dns.ok
[ ] TLS certificate issued                  ← cert_status == "issued"
```

**Response `200`**

```json
{
  "domain": "phoenixdns.app",
  "expected_ip": "129.212.247.213",
  "dns": {
    "ok": true,
    "resolved": ["129.212.247.213"]
  },
  "cert_status": "issued",
  "ready": true,
  "step": "active"
}
```

**`step` values**

| Value          | Meaning                                          |
| -------------- | ------------------------------------------------ |
| `pending_dns`  | A record not yet pointing to the VPS IP          |
| `pending_cert` | DNS is correct but TLS cert not issued yet       |
| `active`       | DNS correct + cert issued — domain is fully live |

**DNS not yet propagated example**

```json
{
  "domain": "phoenixdns.app",
  "expected_ip": "129.212.247.213",
  "dns": {
    "ok": false,
    "resolved": ["1.2.3.4"]
  },
  "cert_status": "pending",
  "ready": false,
  "step": "pending_dns"
}
```

**DNS correct, cert pending example**

```json
{
  "domain": "phoenixdns.app",
  "expected_ip": "129.212.247.213",
  "dns": {
    "ok": true,
    "resolved": ["129.212.247.213"]
  },
  "cert_status": "pending",
  "ready": false,
  "step": "pending_cert"
}
```

**Errors**

| Code  | Reason                            |
| ----- | --------------------------------- |
| `400` | No custom domain set on this site |
| `404` | Site not found                    |

**Typical UI polling loop**

```js
// Poll every 5s until ready
const poll = setInterval(async () => {
  const res = await fetch(`/api/sites/${site}/domain/status`, {
    headers: { "X-API-Key": key },
  });
  const data = await res.json();
  updateUI(data.step, data.dns, data.cert_status);
  if (data.ready) clearInterval(poll);
}, 5000);
```

---

## `POST /api/sites/:site/cert-retry`

Forces a Caddy reload to re-queue ACME certificate issuance for the site's
active domain, then polls for up to 30 s and returns the current cert status.

Use this when:

- `cert_status` has been `"pending"` for more than ~10 minutes
- A `warnings` entry says the snippet is stale or the domain is missing
- After fixing a DNS or port 80 issue

**Response `200`**

```json
{
  "site": "mysite",
  "domain": "example.com",
  "cert_status": "issued"
}
```

**Errors**

| Code  | Reason                              |
| ----- | ----------------------------------- |
| `404` | Site not found                      |
| `409` | Site is not ACTIVE or DOMAIN_ACTIVE |
| `500` | Caddy reload failed                 |

---

## `GET /api/jobs/:id`

Returns the current state of a provisioning or destroy job.

**Response `200`**

```json
{
  "job_id": "63c83c3f-775d-40b5-96ad-567c3f1e10d3",
  "type": "provision",
  "site": "mysite",
  "status": "COMPLETED",
  "attempts": 1,
  "max_attempts": 3,
  "error": "",
  "created_at": "2026-02-25T02:56:20Z",
  "started_at": "2026-02-25T02:56:21Z",
  "completed_at": "2026-02-25T02:56:52Z"
}
```

**`type` values**

| Value              | Triggered by                 |
| ------------------ | ---------------------------- |
| `provision`        | `POST /api/provision`        |
| `static_provision` | `POST /api/static/provision` |
| `destroy`          | `POST /api/destroy`          |

**`status` values**

| Value        | Meaning                             |
| ------------ | ----------------------------------- |
| `PENDING`    | Queued, not yet picked up by worker |
| `PROCESSING` | Worker is actively running the job  |
| `COMPLETED`  | Job finished successfully           |
| `FAILED`     | All retry attempts exhausted        |

**Errors**

| Code  | Reason        |
| ----- | ------------- |
| `404` | Job not found |

---

## `DELETE /api/jobs/:id`

Hard deletes a job record from the database. Job must be in `COMPLETED` or
`FAILED` status.

**Response `200`**

```json
{ "deleted": "63c83c3f-775d-40b5-96ad-567c3f1e10d3" }
```

**Errors**

| Code  | Reason                                      |
| ----- | ------------------------------------------- |
| `404` | Job not found                               |
| `409` | Job is PENDING or PROCESSING (still active) |

---

## Site Status Lifecycle

```
                  POST /provision
                        │
                        ▼
                  PROVISIONING ──── job FAILED ──▶ FAILED (manual cleanup needed)
                        │
                   job COMPLETED
                        │
                        ▼
                     ACTIVE ◀──── DELETE /api/sites/:site/domain
                        │
          POST /api/sites/:site/domain
                        │
                        ▼
                 DOMAIN_ACTIVE
                        │
            POST /api/destroy (either status)
                        │
                        ▼
                  DESTROYING ──── job FAILED ──▶ FAILED
                        │
                   job COMPLETED
                        │
                        ▼
                  DESTROYED
                        │
          DELETE /api/sites/:site (hard delete)
                        │
                        ▼
                   (record gone)
```

---

## Common Workflows

### Provision a WordPress site and wait for it to be ready

```bash
# 1. Queue provisioning
RESP=$(curl -s -X POST https://api.cowsaidmoo.tech/api/provision \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"site":"mysite"}')
JOB_ID=$(echo $RESP | jq -r .job_id)

# 2. Poll until COMPLETED
watch -n 5 curl -s -H "X-API-Key: $KEY" \
  https://api.cowsaidmoo.tech/api/jobs/$JOB_ID | jq .status

# 3. Verify infra is healthy
curl -s -H "X-API-Key: $KEY" \
  https://api.cowsaidmoo.tech/api/sites/mysite | jq '{cert_status,warnings}'
```

### Set a custom domain and watch it go live

```bash
# Step 1 — tell the user to set their A record, then POST the domain
# (the API validates DNS is already pointing before accepting)
curl -s -X POST https://api.cowsaidmoo.tech/api/sites/mysite/domain \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"domain":"example.com"}' | jq '{custom_domain,cert_status}'

# Step 2 — poll domain/status from the UI until ready=true
# step: pending_dns → pending_cert → active
watch -n 5 'curl -s -H "X-API-Key: $KEY" \
  https://api.cowsaidmoo.tech/api/sites/mysite/domain/status \
  | jq "{step,dns_ok: .dns.ok, cert_status, ready}"'
```

**UI response progression:**

```
step=pending_dns  → show "Point example.com → 129.212.247.213"
step=pending_cert → show "DNS verified ✓  Waiting for TLS cert..."
step=active       → show "Domain live ✓"
```

### Force cert issuance if stuck

```bash
curl -s -X POST https://api.cowsaidmoo.tech/api/sites/mysite/cert-retry \
  -H "X-API-Key: $KEY" | jq .
```

### Destroy a site completely

```bash
# 1. Queue destroy
JOB_ID=$(curl -s -X POST https://api.cowsaidmoo.tech/api/destroy \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"site":"mysite"}' | jq -r .job_id)

# 2. Wait for COMPLETED, then hard delete the record
curl -s -X DELETE https://api.cowsaidmoo.tech/api/sites/mysite \
  -H "X-API-Key: $KEY"
```
