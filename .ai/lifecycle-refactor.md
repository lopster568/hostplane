# Control-Plane Lifecycle Refactor — Architectural Analysis & Proposal

## Part 1: Current State Audit

### 1. DB State Mutated Before Infra Success

**`handleSetCustomDomain`** ([api.go](../api.go#L47))

```go
// DB committed FIRST — before any infra mutation succeeds
if err := a.db.SetCustomDomain(site, req.Domain); err != nil { ... }

// These can all fail AFTER the DB says domain is set:
if err := a.regenerateNginx(site, existing.Domain, req.Domain); err != nil { ... }
if err := addTunnelRoute(req.Domain); err != nil { ... }
if err := updateCloudflaredConfig(req.Domain); err != nil { ... }
```

If `addTunnelRoute` fails, the DB says the custom domain is active, nginx is configured for it, but no tunnel route exists. The site is unreachable on the custom domain, but the system believes it's live. There is no compensation — the handler returns 500 and the DB retains the stale domain value.

**`handleRemoveCustomDomain`** ([api.go](../api.go#L84))

Identical pattern. `RemoveCustomDomain` is committed before nginx regen, tunnel removal, or cloudflared cleanup. A failure at any subsequent step leaves the DB claiming no custom domain while infra still routes it.

**Impact:** Every restart, every retry, every health check operates on a DB that may not reflect reality. The cleanup.sh script partially compensates for this manually, which confirms the drift is a known operational pain point.

---

### 2. Missing Idempotency Protections

**`handleSetCustomDomain`** — No uniqueness check:

- No validation that another site doesn't already claim this domain.
- No check that the domain is already set to the requested value (calling it twice creates redundant infra mutations).
- No domain format validation (no TLD check, no wildcard rejection, no subdomain-of-base-domain rejection).

**`handleStaticProvision`** ([api.go](../api.go#L122)) — Missing guards that `handleProvision` has:

- No `HasActiveJob` check — a second provision request for the same site can be queued while one is processing.
- No check for existing ACTIVE site — can overwrite a live static site without destroying it first.

**`StaticProvisioner.Run`** ([static_provisioner.go](../static_provisioner.go#L26)) — No rollback:

- `Provisioner.Run` has a full rollback function tracking `dbCreated`, `volCreated`, `containerCreated`, `nginxWritten`.
- `StaticProvisioner.Run` has zero rollback. If `createContainer` succeeds but `writeNginxConfig` fails, an orphaned container and volume persist with no cleanup path.

**Worker retry** ([worker.go](../worker.go#L60)):

- When a job is retried, `Provisioner.Run` executes again from step 1.
- `createDatabase` uses `IF NOT EXISTS` — safe.
- `createVolume` — Docker volume create is idempotent — safe.
- `createContainer` — has `ContainerInspect` guard — safe.
- But `writeNginxConfig` + `reloadNginx` re-executes unconditionally. If the previous attempt failed after nginx write but before reload, the retry writes the same config again (harmless) but then reloads (also harmless). This path is accidentally idempotent but not designed to be.

---

### 3. Lack of Domain Validation Before Activation

**`handleSetCustomDomain`** performs zero DNS validation:

- No check that the domain resolves at all.
- No check that the domain points to a Cloudflare IP or expected CNAME.
- No check that Cloudflare proxy status is enabled.
- The domain is immediately written to DB, nginx config, tunnel route, and cloudflared config.

The `health.sh` script ([scripts/health.sh](../scripts/health.sh#L51)) retrospectively checks DNS resolution and tunnel config — this is an after-the-fact audit, not a gate. Users can set a domain, get a 200 OK, and traffic will black-hole because DNS isn't pointed yet and there's no mechanism to detect or communicate this.

**What should happen:** Domain setting should transition the site to a `DOMAIN_PENDING_DNS` state, validate resolution asynchronously, and only activate routing after confirmation.

---

### 4. Config Patching Logic That Should Be Full-Regeneration

**`updateCloudflaredConfig`** ([tunnel.go](../tunnel.go#L67)):

```go
// Reads existing file, appends one rule before catch-all, writes back
cfg, err := loadCloudflaredConfig()
newRule := IngressRule{Hostname: domain, Service: "http://10.10.0.10:8080"}
cfg.Ingress = append(cfg.Ingress[:len(cfg.Ingress)-1], newRule, catchAll)
saveCloudflaredConfig(cfg)
```

This is incremental patching. If the config file is manually edited, becomes corrupted, or has stale entries from a failed removal, the patching perpetuates the corruption. The config file is treated as a co-equal source of truth alongside the DB, which violates the single-source-of-truth principle.

**`removeCloudflaredConfig`** — same pattern: filter one entry, rewrite.

**`saveCloudflaredConfig`** ([tunnel.go](../tunnel.go#L37)) — manual string-builder YAML serialization:

```go
var sb strings.Builder
sb.WriteString(fmt.Sprintf("tunnel: %s\n", cfg.Tunnel))
// ...hand-rolled YAML
```

This bypasses `yaml.Marshal` and is fragile. A hostname containing YAML-special characters would break the config.

**`cleanup.sh`** ([scripts/cleanup.sh](../scripts/cleanup.sh#L47)) — uses `sed -i` to patch cloudflared config:

```bash
sed -i "/hostname: $CUSTOM/,+1d" /etc/cloudflared/config.yml
```

Regex-based config patching on a YAML file. If the format changes, this silently fails or corrupts.

**The fix:** The DB should be the sole source of domain→service mappings. A single `regenerateCloudflaredConfig()` function should query the DB for all active custom domains and write the complete config file from scratch every time. Same for nginx: a `regenerateAllNginxConfigs()` should be possible, not just per-site writes.

---

### 5. Missing Rollback Logic on Partial Failure

**`handleSetCustomDomain`** — three infra mutations, zero rollback:

| Step                      | What happens on failure             | Compensation |
| ------------------------- | ----------------------------------- | ------------ |
| `SetCustomDomain` (DB)    | Returns 500, domain persists in DB  | ❌ None      |
| `regenerateNginx`         | DB has domain, nginx doesn't        | ❌ None      |
| `addTunnelRoute`          | DB + nginx have domain, no tunnel   | ❌ None      |
| `updateCloudflaredConfig` | DB + nginx + tunnel, no cloudflared | ❌ None      |

Compare with `Provisioner.Run` ([provisioner.go](../provisioner.go#L25)) which tracks each step with booleans and has a `rollback()` closure. The custom domain handler has no equivalent.

**`handleRemoveCustomDomain`** — same: three infra teardowns, no rollback on partial failure.

**`StaticProvisioner.Run`** — five steps, zero rollback (versus `Provisioner.Run` which has full rollback).

**`Destroyer.Run`** ([destroyer.go](../destroyer.go#L22)) — sequential teardown, no partial failure handling. If `removeVolume` fails after `removeContainer` succeeds, the container is gone but the volume persists. The job is marked FAILED, but retrying it will try to remove a container that no longer exists (handled by `IsErrNotFound` check — accidentally idempotent) and then retry volume removal.

---

### 6. Hard-Coded Assumptions About Tunnel or Nginx State

**IP `10.10.0.10`** appears in:

- [provisioner.go](../provisioner.go#L101) — `CREATE USER ... @'10.10.0.10'`
- [tunnel.go](../tunnel.go#L72) — `Service: "http://10.10.0.10:8080"`
- [config.go](../config.go#L35) — `DockerHost` default
- [scripts/cleanup.sh](../scripts/cleanup.sh), [scripts/health.sh](../scripts/health.sh)

If the app server IP changes, multiple files across Go and Bash must be updated in lockstep.

**`/etc/cloudflared/config.yml`** — hard-coded in [tunnel.go](../tunnel.go#L12):

```go
const cloudflaredConfig = "/etc/cloudflared/config.yml"
```

Not configurable. Cannot be tested without writing to this system path.

**Container naming conventions** — scattered with no central definition:

- `"php_" + site` — [provisioner.go](../provisioner.go), [destroyer.go](../destroyer.go), [scripts/health.sh](../scripts/health.sh)
- `"static_" + site` — [static_provisioner.go](../static_provisioner.go), [scripts/health.sh](../scripts/health.sh)
- `"wp_" + site` — [provisioner.go](../provisioner.go), [destroyer.go](../destroyer.go)
- `"vol_" + site` — [provisioner.go](../provisioner.go), [destroyer.go](../destroyer.go)
- `"static_vol_" + site` — [static_provisioner.go](../static_provisioner.go)

**`"wp_backend"` network** — hard-coded in both [provisioner.go](../provisioner.go#L147) and [static_provisioner.go](../static_provisioner.go#L126).

**`"wordpress:php8.2-fpm"` and `"nginx:stable"`** — hard-coded image tags with no pinning strategy.

**Async cloudflared restart** — [tunnel.go](../tunnel.go#L84):

```go
go func() {
    time.Sleep(100 * time.Millisecond)
    restartCloudflared()
}()
```

Fire-and-forget. No confirmation. No error propagation. The handler returns 200 OK while cloudflared may fail to restart. Contrast with `removeCloudflaredConfig` which does a synchronous restart — inconsistent behavior on the same code path.

---

### 7. Areas Where Infra State and DB State Can Drift

| Scenario                                               | DB State                  | Infra State                         | Detection                               |
| ------------------------------------------------------ | ------------------------- | ----------------------------------- | --------------------------------------- |
| `SetCustomDomain` → nginx OK → tunnel fails            | `custom_domain = "x.com"` | Nginx has config, no tunnel route   | None (manual health.sh)                 |
| `Provisioner.Run` OK → `CompleteJob` DB call fails     | `status = PROVISIONING`   | Fully provisioned container + nginx | Worker retries, accidentally idempotent |
| `Destroyer.Run` OK → `CompleteJob` DB call fails       | `status = DESTROYING`     | All infra removed                   | Worker retries, destroyer is idempotent |
| Cloudflared restart fails (async fire-and-forget)      | `custom_domain = "x.com"` | Config written but not loaded       | None                                    |
| Manual `sed` edit to cloudflared config via cleanup.sh | DB unchanged              | Config modified                     | None                                    |
| Container crashes and doesn't restart                  | `status = ACTIVE`         | Container in exited state           | health.sh only                          |
| Nginx config manually removed                          | `status = ACTIVE`         | No routing                          | health.sh only                          |

**Root cause:** There is no reconciliation loop. The system is entirely event-driven (API call → job → execute → done). If any step fails silently or the system crashes mid-operation, state drifts until a human runs health.sh or cleanup.sh.

---

## Part 2: Proposed Architecture

### 1. SiteStatus Enum with Explicit State Transitions

```go
type SiteStatus string

const (
    SiteCreated          SiteStatus = "CREATED"           // Record exists, no infra
    SiteProvisioning     SiteStatus = "PROVISIONING"      // Infra creation in progress
    SiteActive           SiteStatus = "ACTIVE"            // Fully provisioned, serving traffic
    SiteDomainPending    SiteStatus = "DOMAIN_PENDING"    // Custom domain requested, awaiting DNS validation
    SiteDomainValidating SiteStatus = "DOMAIN_VALIDATING" // DNS check in progress
    SiteDomainRouting    SiteStatus = "DOMAIN_ROUTING"    // DNS validated, applying tunnel + nginx
    SiteDomainActive     SiteStatus = "DOMAIN_ACTIVE"     // Custom domain fully live
    SiteDomainRemoving   SiteStatus = "DOMAIN_REMOVING"   // Tearing down custom domain routing
    SiteDestroying       SiteStatus = "DESTROYING"        // Infra teardown in progress
    SiteDestroyed        SiteStatus = "DESTROYED"         // Infra removed, record retained
    SiteFailed           SiteStatus = "FAILED"            // Terminal failure, requires intervention
)

// Allowed transitions — the only legal state changes.
var allowedTransitions = map[SiteStatus][]SiteStatus{
    SiteCreated:          {SiteProvisioning},
    SiteProvisioning:     {SiteActive, SiteFailed},
    SiteActive:           {SiteDomainPending, SiteDestroying},
    SiteDomainPending:    {SiteDomainValidating, SiteActive},       // can cancel back to ACTIVE
    SiteDomainValidating: {SiteDomainRouting, SiteDomainPending},   // retry validation
    SiteDomainRouting:    {SiteDomainActive, SiteActive},           // rollback to ACTIVE on failure
    SiteDomainActive:     {SiteDomainRemoving, SiteDestroying},
    SiteDomainRemoving:   {SiteActive, SiteFailed},
    SiteDestroying:       {SiteDestroyed, SiteFailed},
    SiteFailed:           {SiteProvisioning, SiteDestroying},       // manual recovery paths
}

func (from SiteStatus) CanTransitionTo(to SiteStatus) bool {
    for _, allowed := range allowedTransitions[from] {
        if allowed == to {
            return true
        }
    }
    return false
}
```

**Key properties:**

- No implicit jumps. `.CanTransitionTo()` is called before every DB update.
- Domain lifecycle is modeled as explicit states, not a boolean column flip.
- `FAILED` has explicit recovery paths (re-provision or force-destroy).
- Every state implies a specific set of infra expectations that the reconciler can verify.

**DB migration:**

```sql
ALTER TABLE sites
  MODIFY COLUMN status ENUM(
    'CREATED','PROVISIONING','ACTIVE',
    'DOMAIN_PENDING','DOMAIN_VALIDATING','DOMAIN_ROUTING','DOMAIN_ACTIVE',
    'DOMAIN_REMOVING','DESTROYING','DESTROYED','FAILED'
  ) NOT NULL DEFAULT 'CREATED',
  ADD COLUMN desired_custom_domain VARCHAR(255) DEFAULT NULL,
  ADD COLUMN validated_at TIMESTAMP NULL,
  ADD COLUMN last_reconciled_at TIMESTAMP NULL,
  ADD UNIQUE INDEX idx_custom_domain (custom_domain);
```

The `desired_custom_domain` column stores what the user requested. The `custom_domain` column stores what's actually been validated and routed. These are different columns intentionally — desired vs. confirmed.

---

### 2. Refactored Handler Pattern: Validate → Apply Infra → Confirm → Commit State

**Current pattern (broken):**

```
Validate input → Commit DB → Try infra → Hope it works
```

**Proposed pattern:**

```
Validate input → Validate preconditions → Transition to intent state →
  → (async worker) Apply infra → Verify infra → Commit final state
```

#### Custom Domain — Refactored

**API handler** — only validates and records intent:

```go
func (a *API) handleSetCustomDomain(c *gin.Context) {
    site := c.Param("site")
    var req struct {
        Domain string `json:"domain" binding:"required"`
    }
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(400, gin.H{"error": "domain is required"})
        return
    }

    // 1. Validate domain format
    if err := validateDomainFormat(req.Domain); err != nil {
        c.JSON(400, gin.H{"error": err.Error()})
        return
    }

    // 2. Validate no other site claims this domain
    if err := a.db.EnsureDomainAvailable(req.Domain, site); err != nil {
        c.JSON(409, gin.H{"error": "domain already claimed"})
        return
    }

    // 3. Validate site is in a state that allows domain attachment
    existing, err := a.db.GetSite(site)
    if err != nil {
        c.JSON(404, gin.H{"error": "site not found"})
        return
    }
    if !SiteStatus(existing.Status).CanTransitionTo(SiteDomainPending) {
        c.JSON(409, gin.H{"error": fmt.Sprintf("cannot set domain in state %s", existing.Status)})
        return
    }

    // 4. Record intent — NOT activation
    if err := a.db.SetDesiredDomain(site, req.Domain); err != nil {
        c.JSON(500, gin.H{"error": "failed to save domain"})
        return
    }
    if err := a.db.TransitionSite(site, SiteDomainPending); err != nil {
        c.JSON(500, gin.H{"error": "failed to update status"})
        return
    }

    c.JSON(202, gin.H{
        "site":   site,
        "domain": req.Domain,
        "status": "DOMAIN_PENDING",
        "message": "Domain recorded. DNS validation will begin automatically.",
    })
}
```

**Worker** — domain lifecycle processing (new job type):

```go
func (w *Worker) processDomainPending(site *Site) error {
    // Step 1: Validate DNS
    if err := w.dnsValidator.Validate(site.DesiredCustomDomain); err != nil {
        log.Printf("[domain] %s: DNS validation failed for %s: %v",
            site.Site, site.DesiredCustomDomain, err)
        // Don't fail — stay in DOMAIN_PENDING for retry on next reconcile tick
        return nil
    }

    // Step 2: Transition to DOMAIN_ROUTING
    if err := w.db.TransitionSite(site.Site, SiteDomainRouting); err != nil {
        return err
    }

    // Step 3: Apply infra (all-or-nothing with rollback)
    if err := w.applyDomainRouting(site); err != nil {
        log.Printf("[domain] %s: routing failed, rolling back to ACTIVE: %v",
            site.Site, err)
        w.db.TransitionSite(site.Site, SiteActive)
        w.db.ClearDesiredDomain(site.Site)
        return err
    }

    // Step 4: Commit — only after infra is confirmed
    if err := w.db.ActivateCustomDomain(site.Site, site.DesiredCustomDomain); err != nil {
        return err
    }
    return w.db.TransitionSite(site.Site, SiteDomainActive)
}

func (w *Worker) applyDomainRouting(site *Site) error {
    domain := site.DesiredCustomDomain

    // Phase 1: Tunnel route
    if err := w.tunnelProvider.RouteDomain(domain); err != nil {
        return fmt.Errorf("tunnel route: %w", err)
    }

    // Phase 2: Regenerate ALL cloudflared config from DB
    if err := w.tunnelProvider.RegenerateConfig(); err != nil {
        // Compensate: remove the route we just added
        w.tunnelProvider.RemoveRoute(domain)
        return fmt.Errorf("cloudflared config: %w", err)
    }

    // Phase 3: Regenerate nginx config for this site
    if err := w.edgeProvider.ApplySite(*site); err != nil {
        // Compensate: revert cloudflared + tunnel
        w.tunnelProvider.RegenerateConfig() // re-gen without the new domain
        w.tunnelProvider.RemoveRoute(domain)
        return fmt.Errorf("nginx config: %w", err)
    }

    return nil
}
```

**Key differences from current code:**

1. API handler never touches infra. It records intent and returns 202.
2. State transitions are validated before execution.
3. Infra mutations have explicit compensation on partial failure.
4. DB final state is committed only after all infra confirms.
5. DNS validation is a gate, not an afterthought.

---

### 3. Idempotent Infrastructure Provider Interfaces

```go
// EdgeProvider manages nginx routing configuration.
// All methods must be safe to call multiple times with the same input.
type EdgeProvider interface {
    // ApplySite writes the nginx config for a single site.
    // Generates the full config from the Site struct (not patching).
    // Calls nginx -t and reload after writing.
    ApplySite(site Site) error

    // RemoveSite removes the nginx config for a site and reloads.
    RemoveSite(siteName string) error

    // RegenerateAll queries the DB for all active sites and regenerates
    // every nginx config file, removing any that shouldn't exist.
    // This is the reconciliation entry point.
    RegenerateAll(sites []Site) error
}

// TunnelProvider manages Cloudflare tunnel routing.
// All methods must be safe to call multiple times with the same input.
type TunnelProvider interface {
    // RouteDomain creates a DNS CNAME for the domain pointing to the tunnel.
    // No-ops if the route already exists.
    RouteDomain(domain string) error

    // RemoveRoute removes a DNS route. No-ops if it doesn't exist.
    RemoveRoute(domain string) error

    // RegenerateConfig rebuilds the entire cloudflared config.yml from
    // the provided list of active domains, replacing the file atomically.
    // Restarts cloudflared synchronously and verifies it's healthy.
    RegenerateConfig() error
}

// DNSValidator checks whether a domain is properly configured.
type DNSValidator interface {
    // Validate checks that the domain resolves and points to expected targets.
    // Returns nil if validated, error describing what's wrong otherwise.
    Validate(domain string) error
}

// InfraProvider composes all infrastructure operations.
type InfraProvider interface {
    // ProvisionSite creates all infrastructure for a new site.
    // Idempotent — safe to call on a partially-provisioned site.
    ProvisionSite(site Site) error

    // DestroySite removes all infrastructure for a site.
    // Idempotent — safe to call on a partially-destroyed site.
    DestroySite(site Site) error
}
```

**Idempotency contracts for each method:**

| Method                            | Idempotency guarantee                                                           |
| --------------------------------- | ------------------------------------------------------------------------------- |
| `EdgeProvider.ApplySite`          | Overwrites config file unconditionally. Nginx test + reload is always safe.     |
| `EdgeProvider.RemoveSite`         | `rm -f` is idempotent. Reload after removal is safe even if file didn't exist.  |
| `EdgeProvider.RegenerateAll`      | Deletes all `.conf` files, writes fresh set, reloads. Deterministic from input. |
| `TunnelProvider.RouteDomain`      | Checks if CNAME exists first. Returns nil if already present.                   |
| `TunnelProvider.RemoveRoute`      | No-op if route doesn't exist.                                                   |
| `TunnelProvider.RegenerateConfig` | Atomic file write (write to `.tmp`, rename). Full config from DB, not patching. |
| `DNSValidator.Validate`           | Pure read — no side effects.                                                    |

**Cloudflared config — full regeneration:**

```go
func (t *CloudflareTunnelProvider) RegenerateConfig() error {
    // 1. Query DB for all domains that should be routed
    sites, err := t.db.ListSitesWithCustomDomains()
    if err != nil {
        return fmt.Errorf("list domains: %w", err)
    }

    // 2. Build complete config from scratch
    cfg := CloudflaredConfig{
        Tunnel:          t.tunnelID,
        CredentialsFile: t.credentialsFile,
    }
    for _, s := range sites {
        cfg.Ingress = append(cfg.Ingress, IngressRule{
            Hostname: s.CustomDomain,
            Service:  t.serviceTarget,
        })
    }
    // Catch-all must always be last
    cfg.Ingress = append(cfg.Ingress, IngressRule{Service: "http_status:404"})

    // 3. Atomic write
    tmpPath := t.configPath + ".tmp"
    data, err := yaml.Marshal(cfg)
    if err != nil {
        return fmt.Errorf("marshal config: %w", err)
    }
    if err := os.WriteFile(tmpPath, data, 0644); err != nil {
        return fmt.Errorf("write tmp config: %w", err)
    }
    if err := os.Rename(tmpPath, t.configPath); err != nil {
        return fmt.Errorf("rename config: %w", err)
    }

    // 4. Synchronous restart with health check
    if err := t.restart(); err != nil {
        return fmt.Errorf("restart cloudflared: %w", err)
    }

    return nil
}
```

---

### 4. Reconciliation Loop Design

The reconciler runs as a background goroutine alongside the existing job worker. It has a separate tick interval (e.g., every 60 seconds) and is **read-heavy, write-cautious**.

```go
type Reconciler struct {
    db             *DB
    edgeProvider   EdgeProvider
    tunnelProvider TunnelProvider
    docker         *client.Client
    cfg            Config
    interval       time.Duration
}

func (r *Reconciler) Start(ctx context.Context) {
    ticker := time.NewTicker(r.interval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            log.Println("[reconciler] shutting down")
            return
        case <-ticker.C:
            r.reconcile()
        }
    }
}

func (r *Reconciler) reconcile() {
    log.Println("[reconciler] starting sweep")
    start := time.Now()

    sites, err := r.db.ListSites()
    if err != nil {
        log.Printf("[reconciler] failed to list sites: %v", err)
        return
    }

    var drifts []DriftReport

    for _, site := range sites {
        switch SiteStatus(site.Status) {
        case SiteActive, SiteDomainActive:
            d := r.checkActiveSite(site)
            if d != nil {
                drifts = append(drifts, *d)
            }
        case SiteProvisioning:
            r.checkStuckProvisioning(site)
        case SiteDomainPending:
            r.processDomainPending(site)
        case SiteDomainValidating:
            r.processDomainValidation(site)
        case SiteDestroying:
            r.checkStuckDestroying(site)
        }
    }

    // Report and optionally repair drifts
    for _, d := range drifts {
        log.Printf("[reconciler] DRIFT site=%s type=%s detail=%s",
            d.Site, d.Type, d.Detail)
        if d.AutoRepairable {
            r.repair(d)
        }
    }

    r.db.UpdateReconcileTimestamp(time.Now())
    log.Printf("[reconciler] sweep completed in %s, drifts=%d", time.Since(start), len(drifts))
}
```

**What the reconciler checks for ACTIVE sites:**

```go
type DriftType string

const (
    DriftContainerMissing  DriftType = "CONTAINER_MISSING"
    DriftContainerStopped  DriftType = "CONTAINER_STOPPED"
    DriftNginxConfigMissing DriftType = "NGINX_CONFIG_MISSING"
    DriftTunnelMissing     DriftType = "TUNNEL_ROUTE_MISSING"
    DriftOrphanedContainer DriftType = "ORPHANED_CONTAINER"
    DriftOrphanedNginx     DriftType = "ORPHANED_NGINX_CONFIG"
    DriftOrphanedTunnel    DriftType = "ORPHANED_TUNNEL_ROUTE"
)

type DriftReport struct {
    Site           string
    Type           DriftType
    Detail         string
    AutoRepairable bool
}

func (r *Reconciler) checkActiveSite(site Site) *DriftReport {
    // 1. Container should be running
    containerName := r.containerName(site)
    info, err := r.docker.ContainerInspect(context.Background(), containerName)
    if client.IsErrNotFound(err) {
        return &DriftReport{
            Site: site.Site, Type: DriftContainerMissing,
            Detail: containerName + " not found",
            AutoRepairable: false, // requires re-provision
        }
    }
    if info.State.Status != "running" {
        return &DriftReport{
            Site: site.Site, Type: DriftContainerStopped,
            Detail: containerName + " status: " + info.State.Status,
            AutoRepairable: true, // can restart
        }
    }

    // 2. Nginx config should exist
    // 3. If DOMAIN_ACTIVE, tunnel route should exist
    // ... etc

    return nil
}
```

**Reconciler replaces:**

- Manual `health.sh` — automated drift detection.
- Manual `cleanup.sh` — automated orphan cleanup.
- The implicit "retry and hope" strategy — explicit repair.

**Reconciler does NOT replace:**

- The job worker — jobs still handle explicit user-requested state changes.
- The reconciler handles drift repair and async lifecycle advancement (domain validation).

---

### 5. Logging Improvements for Observability

**Current state:** Unstructured `log.Printf` with inconsistent prefixes (`[worker]`, `[rollback]`, `[main]`). No correlation IDs. No timing. No structured fields.

**Proposed: Structured logging with operation context.**

```go
type OpContext struct {
    Site      string     `json:"site"`
    JobID     string     `json:"job_id,omitempty"`
    Operation string     `json:"op"`
    Step      string     `json:"step"`
    StartedAt time.Time  `json:"started_at"`
}

func (o *OpContext) Log(level, msg string, fields ...any) {
    // In production, this would use slog or zerolog.
    // Simplified for illustration:
    elapsed := time.Since(o.StartedAt)
    log.Printf("[%s] site=%s job=%s op=%s step=%s elapsed=%s %s %v",
        level, o.Site, o.JobID, o.Operation, o.Step, elapsed, msg, fields)
}
```

**Every lifecycle step must log:**

1. **Intent** — "Starting nginx config write for site=foo domain=bar.com"
2. **Outcome** — "Nginx config written successfully" or "Nginx config write FAILED: permission denied"
3. **External response** — "cloudflared route dns returned: already exists (treated as success)"
4. **Timing** — elapsed time per step and total operation time.
5. **Correlation** — job ID and site name on every log line.

**Example output after refactor:**

```
[INFO] site=testsite job=abc-123 op=provision step=create_db elapsed=0s starting database creation
[INFO] site=testsite job=abc-123 op=provision step=create_db elapsed=1.2s database wp_testsite created
[INFO] site=testsite job=abc-123 op=provision step=create_volume elapsed=1.2s starting volume creation
[INFO] site=testsite job=abc-123 op=provision step=create_volume elapsed=1.8s volume vol_testsite created
[INFO] site=testsite job=abc-123 op=provision step=create_container elapsed=1.8s starting container creation
[INFO] site=testsite job=abc-123 op=provision step=create_container elapsed=4.1s container php_testsite started
[INFO] site=testsite job=abc-123 op=provision step=nginx_config elapsed=4.1s writing nginx config
[INFO] site=testsite job=abc-123 op=provision step=nginx_reload elapsed=4.3s nginx test passed, reloading
[INFO] site=testsite job=abc-123 op=provision step=complete elapsed=4.5s provision completed successfully
```

**Metrics to add (when ready):**

- `hostplane_jobs_total{type, status}` — counter
- `hostplane_job_duration_seconds{type}` — histogram
- `hostplane_reconciler_drifts_total{type}` — counter
- `hostplane_reconciler_repairs_total{type, success}` — counter
- `hostplane_active_sites` — gauge

---

### 6. Test Strategy for Lifecycle Transitions

**Layer 1: State machine unit tests (pure logic, no infra)**

```go
func TestStateTransitions(t *testing.T) {
    tests := []struct {
        from    SiteStatus
        to      SiteStatus
        allowed bool
    }{
        {SiteCreated, SiteProvisioning, true},
        {SiteCreated, SiteActive, false},              // cannot skip provisioning
        {SiteActive, SiteDomainPending, true},
        {SiteActive, SiteDomainActive, false},          // cannot skip validation
        {SiteDomainActive, SiteActive, false},           // must go through DOMAIN_REMOVING
        {SiteDomainActive, SiteDomainRemoving, true},
        {SiteFailed, SiteProvisioning, true},            // recovery
        {SiteFailed, SiteDestroying, true},              // force destroy
        {SiteFailed, SiteActive, false},                 // cannot jump to ACTIVE
        {SiteDestroyed, SiteActive, false},              // cannot resurrect
    }

    for _, tt := range tests {
        t.Run(fmt.Sprintf("%s→%s", tt.from, tt.to), func(t *testing.T) {
            got := tt.from.CanTransitionTo(tt.to)
            if got != tt.allowed {
                t.Errorf("expected %v, got %v", tt.allowed, got)
            }
        })
    }
}
```

**Layer 2: Provider interface tests with mock infra**

```go
type MockEdgeProvider struct {
    AppliedSites  []string
    RemovedSites  []string
    FailOnApply   error
}

func (m *MockEdgeProvider) ApplySite(site Site) error {
    if m.FailOnApply != nil {
        return m.FailOnApply
    }
    m.AppliedSites = append(m.AppliedSites, site.Site)
    return nil
}

// Test: partial failure triggers rollback
func TestDomainRoutingRollbackOnNginxFailure(t *testing.T) {
    mockEdge := &MockEdgeProvider{FailOnApply: fmt.Errorf("nginx write failed")}
    mockTunnel := &MockTunnelProvider{}
    mockDB := NewMockDB()

    worker := &Worker{
        db:             mockDB,
        edgeProvider:   mockEdge,
        tunnelProvider: mockTunnel,
    }

    site := &Site{Site: "test", DesiredCustomDomain: "custom.com", Status: "DOMAIN_ROUTING"}
    err := worker.applyDomainRouting(site)

    assert.Error(t, err)
    assert.Contains(t, mockTunnel.RemovedRoutes, "custom.com")  // tunnel route was compensated
    assert.Equal(t, "ACTIVE", mockDB.GetStatus("test"))          // state rolled back
}
```

**Layer 3: Integration tests with real Docker (CI environment)**

```go
func TestFullProvisioningLifecycle(t *testing.T) {
    if testing.Short() {
        t.Skip("integration test")
    }

    db := setupTestDB(t)
    docker := setupTestDocker(t)
    cfg := testConfig()

    provisioner := NewProvisioner(docker, cfg)
    destroyer := NewDestroyer(docker, cfg)

    site := "integtest" + randomSuffix()

    // Provision
    err := provisioner.Run(site)
    require.NoError(t, err)

    // Verify container running
    info, err := docker.ContainerInspect(context.Background(), "php_"+site)
    require.NoError(t, err)
    assert.Equal(t, "running", info.State.Status)

    // Verify idempotency — run again
    err = provisioner.Run(site)
    require.NoError(t, err)

    // Destroy
    err = destroyer.Run(site)
    require.NoError(t, err)

    // Verify container gone
    _, err = docker.ContainerInspect(context.Background(), "php_"+site)
    assert.True(t, client.IsErrNotFound(err))

    // Verify destroy is idempotent
    err = destroyer.Run(site)
    require.NoError(t, err)
}
```

**Layer 4: Reconciler convergence tests**

```go
func TestReconcilerRepairsStoppedContainer(t *testing.T) {
    // Setup: ACTIVE site with stopped container
    mockDocker := &MockDockerClient{
        Containers: map[string]ContainerState{
            "php_test": {Status: "exited"},
        },
    }

    reconciler := &Reconciler{docker: mockDocker, ...}
    drifts := reconciler.checkActiveSite(Site{Site: "test", Status: "ACTIVE"})

    assert.Equal(t, DriftContainerStopped, drifts.Type)
    assert.True(t, drifts.AutoRepairable)

    // Run repair
    reconciler.repair(*drifts)

    // Verify container restarted
    assert.Equal(t, "running", mockDocker.Containers["php_test"].Status)
}
```

**Test coverage priorities:**

1. State transition validation — exhaustive (this is the contract).
2. Rollback paths — every partial failure scenario for provision, destroy, domain set, domain remove.
3. Idempotency — call every provider method twice, assert identical outcome.
4. Reconciler drift detection — one test per `DriftType`.
5. Reconciler repair — one test per auto-repairable drift.
6. Integration — full lifecycle provision → domain → destroy with real Docker.

---

## Part 3: Implementation Sequence

> **Last updated: 2026-02-23**
> Build status: `go build` ✅ `go vet` ✅

### Phase 1 — Foundation (no behavior change)

1. ✅ Extract naming conventions to central `naming.go` (`ContainerName(site, type)`, `VolumeName(site, type)`, etc.).
2. ✅ Move hard-coded IPs/paths to `Config` struct (`AppServerIP`, `DockerNetwork`, `CloudflaredConfigPath`, `TunnelName`, `ServiceTarget`).
3. ✅ Add `SiteStatus` type with `CanTransitionTo()` in `lifecycle.go` (11 states, full transition map).
4. ✅ Add `TransitionSite()` DB method that validates transitions before writing.
5. ⬜ Replace raw `UpdateSiteStatus()` calls with `TransitionSite()` — `TransitionSite` exists but callers (`CompleteJob`, `FailJob`) still use `UpdateSiteStatus` directly. These need to be migrated, but requires ensuring the status values in the DB match the new enum first (DB migration).
6. ⬜ Add structured logging wrapper — `lifecycle.go` has validation logging, tunnel.go has operation logging, but no central `OpContext` struct or `slog` integration yet.

### Phase 2 — Infrastructure interfaces

7. ⬜ Define `EdgeProvider`, `TunnelProvider`, `DNSValidator` interfaces — `TunnelManager` exists as a concrete struct but not behind an interface. No `EdgeProvider` interface yet.
8. ⬜ Implement `DockerEdgeProvider` wrapping current nginx logic.
9. ✅ _Partial:_ `TunnelManager` uses `yaml.Marshal` + atomic write + sync restart. But does NOT yet have full-regeneration from DB (`RegenerateAll` method querying `ListCustomDomains()`). It still patches incrementally.
10. ✅ _Partial:_ `ValidateDomainDNS()` exists in `lifecycle.go` (resolution check). No Cloudflare IP range verification yet.
11. ⬜ Replace direct nginx/tunnel calls in provisioner/destroyer with interface calls.

### Phase 3 — Domain lifecycle

12. ⬜ Add `desired_custom_domain` column and domain-specific states — DB migration not applied. The `SiteStatus` enum is defined in Go but the MySQL column is still a plain `VARCHAR`/free-text field.
13. ⬜ Refactor `handleSetCustomDomain` to record-intent-only (returns 202) — currently it still applies infra synchronously in the API handler (but now with correct ordering: infra-first, DB-last, with rollback). Moving to async/worker-driven is Phase 3.
14. ⬜ Add domain lifecycle processing to worker (PENDING → VALIDATING → ROUTING → ACTIVE).
15. ✅ Add rollback on partial domain routing failure — `handleSetCustomDomain` now rolls back nginx if tunnel fails, rolls back nginx+tunnel if cloudflared fails.
16. ✅ _Partial:_ `handleRemoveCustomDomain` has infra-first ordering with rollback, but is still synchronous in handler (not worker-driven).

### Phase 4 — Reconciliation

17. ⬜ Implement `Reconciler` with drift detection for ACTIVE sites.
18. ⬜ Add orphan detection (containers, nginx configs, tunnel routes without DB records).
19. ⬜ Add auto-repair for safe drifts (stopped containers, missing nginx configs).
20. ⬜ Replace `health.sh` and `cleanup.sh` functionality with reconciler + API endpoints.

### Phase 5 — Hardening

21. ✅ Add `StaticProvisioner` rollback (parity with `Provisioner`).
22. ✅ Add idempotency guards to `handleStaticProvision` (match `handleProvision` — `HasActiveJob` + existing ACTIVE check).
23. ✅ Add domain uniqueness constraint — `EnsureDomainAvailable()` DB method + check in `handleSetCustomDomain`.
24. ✅ Remove async fire-and-forget cloudflared restart — all `TunnelManager` methods use synchronous `restartCloudflared()`.
25. ⬜ Add integration test suite.

---

## Part 4: Implementation Progress Log

### Session 1 — 2026-02-23 — Foundation + Critical Fixes

**New files created:**

- `naming.go` — 11 naming functions (`PHPContainerName`, `StaticContainerName`, `VolumeName`, `StaticVolumeName`, `WPDatabaseName`, `WPDatabaseUser`, `WPDatabasePass`, `SiteDomain`, `NginxConfFile`, `ContainerNameForType`, `TmpUploadContainer`)
- `lifecycle.go` — `SiteStatus` enum (11 states), `allowedTransitions` map, `CanTransitionTo()`, `IsValid()`, `IsTerminal()`, `AllowsCustomDomain()`, `AllowsDestroy()`, `ValidateDomainFormat()`, `ValidateDomainNotBase()`, `ValidateCustomDomain()`, `ValidateDomainDNS()`

**Files modified:**

| File                    | Changes                                                                                                                                                                                                                                                                                                                                                                       |
| ----------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `config.go`             | Added 5 new fields: `AppServerIP`, `DockerNetwork`, `CloudflaredConfigPath`, `TunnelName`, `ServiceTarget` with env-var overrides                                                                                                                                                                                                                                             |
| `db.go`                 | Added `TransitionSite()` (validates state machine), `EnsureDomainAvailable()` (domain uniqueness), `ListCustomDomains()` (for future full-regen)                                                                                                                                                                                                                              |
| `tunnel.go`             | Full rewrite → `TunnelManager` struct. Configurable paths from `Config`. `yaml.Marshal` + atomic file write (tmp+rename). Synchronous restart everywhere. Idempotency logging. Removed fire-and-forget `go func()`                                                                                                                                                            |
| `api.go`                | `API` struct now holds `*TunnelManager`. `handleSetCustomDomain`: validate → infra → DB ordering with 3-step rollback. `handleRemoveCustomDomain`: infra removal → DB ordering with rollback. `handleStaticProvision`: added `HasActiveJob` + existing-site guards. Domain format/uniqueness validation. Idempotent no-op when domain already set. Uses `SiteDomain()` naming |
| `provisioner.go`        | All naming via `naming.go` functions. IP via `cfg.AppServerIP`. Network via `cfg.DockerNetwork`                                                                                                                                                                                                                                                                               |
| `destroyer.go`          | All naming via `naming.go` functions. IP via `cfg.AppServerIP`                                                                                                                                                                                                                                                                                                                |
| `static_provisioner.go` | Added full rollback with step tracking (`volCreated`, `containerCreated`, `nginxWritten`). Added `removeNginxConfig()`. All naming via `naming.go` functions. Network via `cfg.DockerNetwork`                                                                                                                                                                                 |
| `main.go`               | Creates `TunnelManager`, passes to `NewAPI()`                                                                                                                                                                                                                                                                                                                                 |

**What is NOT done yet (for next session):**

1. **DB migration** — The MySQL `sites.status` column needs to be migrated to support the new enum values. Until then, `TransitionSite()` works but is not enforced everywhere.
2. **Migrate `CompleteJob`/`FailJob`** to use `TransitionSite()` instead of raw `UpdateSiteStatus()`.
3. **Infrastructure interfaces** (`EdgeProvider`, `TunnelProvider`) — concrete implementations exist but are not behind interfaces. The worker/provisioner still call methods directly. Needed for testability.
4. **Full-regeneration model for cloudflared** — `TunnelManager` still patches incrementally (add/remove one rule). Need a `RegenerateAll()` that writes the entire config from `ListCustomDomains()`.
5. **Async domain lifecycle via worker** — Domain set/remove still runs synchronously in API handlers. Plan calls for moving to intent-recording (202) + worker-driven lifecycle.
6. **Reconciliation loop** — Not started. This is the drift-detection and auto-repair system.
7. **Structured logging** — Operations log more now, but no `slog`/`OpContext` wrapper.
8. **Test suite** — No tests written yet.
9. **`desired_custom_domain` DB column** — Not added. Needed for the async domain lifecycle where "what the user wants" differs from "what's actually routed".
10. **Cloudflare IP range validation** in `ValidateDomainDNS` — currently checks resolution only, not that it points to CF.
