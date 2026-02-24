# Control-Plane Lifecycle Refactor — Context Summary

> **Last updated: 2026-02-23** | Build: `go build` ✅ `go vet` ✅
> Full audit history: see git log for the original 900-line analysis

## Project

Hostplane control-plane — Go binary that orchestrates WordPress/static site provisioning via Docker, nginx, Cloudflare tunnels, and MySQL. Files: `api.go`, `config.go`, `db.go`, `provisioner.go`, `static_provisioner.go`, `destroyer.go`, `tunnel.go`, `worker.go`, `nginx.go`, `main.go`, `naming.go`, `lifecycle.go`.

## Goal

Move from imperative step execution to a lifecycle-driven, state-machine-based architecture with: validated state transitions, infra-before-DB ordering, rollback on partial failure, idempotent infra providers, a reconciliation loop, and structured logging.

---

## What Was Done (Session 1 — 2026-02-23)

### New Files

- **`naming.go`** — 11 functions centralizing all resource naming (`PHPContainerName`, `StaticContainerName`, `VolumeName`, `StaticVolumeName`, `WPDatabaseName`, `WPDatabaseUser`, `WPDatabasePass`, `SiteDomain`, `NginxConfFile`, `ContainerNameForType`, `TmpUploadContainer`).
- **`lifecycle.go`** — `SiteStatus` enum (11 states: `CREATED`, `PROVISIONING`, `ACTIVE`, `DOMAIN_PENDING`, `DOMAIN_VALIDATING`, `DOMAIN_ROUTING`, `DOMAIN_ACTIVE`, `DOMAIN_REMOVING`, `DESTROYING`, `DESTROYED`, `FAILED`), `allowedTransitions` map, `CanTransitionTo()`, helper methods (`IsValid`, `IsTerminal`, `AllowsCustomDomain`, `AllowsDestroy`), domain validation (`ValidateDomainFormat`, `ValidateDomainNotBase`, `ValidateCustomDomain`, `ValidateDomainDNS`).

### Key Changes to Existing Files

| File                    | What changed                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `config.go`             | +5 fields: `AppServerIP`, `DockerNetwork`, `CloudflaredConfigPath`, `TunnelName`, `ServiceTarget` (all env-configurable). No more hard-coded `10.10.0.10` or `/etc/cloudflared/config.yml` in Go code.                                                                                                                                                                                                                                        |
| `db.go`                 | +`TransitionSite()` (validates state machine before write), +`EnsureDomainAvailable()` (domain uniqueness across active sites), +`ListCustomDomains()` (for future full-regen).                                                                                                                                                                                                                                                               |
| `tunnel.go`             | Full rewrite → `TunnelManager` struct. `yaml.Marshal` + atomic file write (tmp → rename). All restarts synchronous (removed fire-and-forget `go func()`). All paths from `Config`. Idempotency checks + logging.                                                                                                                                                                                                                              |
| `api.go`                | `API` struct holds `*TunnelManager`. **`handleSetCustomDomain`**: validate format/uniqueness → apply infra (nginx → tunnel → cloudflared) → commit DB last. 3-step rollback on partial failure. Idempotent no-op if domain already set. **`handleRemoveCustomDomain`**: remove infra first → commit DB last. Rollback if cloudflared removal fails. **`handleStaticProvision`**: +`HasActiveJob` + existing-ACTIVE-site guards (was missing). |
| `provisioner.go`        | All naming via `naming.go`. IP via `cfg.AppServerIP`. Network via `cfg.DockerNetwork`.                                                                                                                                                                                                                                                                                                                                                        |
| `destroyer.go`          | All naming via `naming.go`. IP via `cfg.AppServerIP`.                                                                                                                                                                                                                                                                                                                                                                                         |
| `static_provisioner.go` | +Full rollback (`volCreated`, `containerCreated`, `nginxWritten` tracking + `removeNginxConfig()`). All naming via `naming.go`. Network via `cfg.DockerNetwork`.                                                                                                                                                                                                                                                                              |
| `main.go`               | Creates `TunnelManager`, passes to `NewAPI()`.                                                                                                                                                                                                                                                                                                                                                                                                |

### Original Defects Fixed

1. ✅ **DB-before-infra ordering** — Domain handlers now apply infra first, commit DB last.
2. ✅ **Missing rollback on partial domain failure** — 3-step compensation in set, 2-step in remove.
3. ✅ **StaticProvisioner had no rollback** — Now has parity with Provisioner.
4. ✅ **handleStaticProvision missing guards** — Has `HasActiveJob` + existing-site check.
5. ✅ **Domain uniqueness** — `EnsureDomainAvailable()` + format validation.
6. ✅ **Async fire-and-forget cloudflared restart** — All synchronous now.
7. ✅ **Hard-coded IPs/paths** — All in `Config` with env-var overrides.
8. ✅ **Scattered naming conventions** — Centralized in `naming.go`.
9. ✅ **Hand-rolled YAML serialization** — Uses `yaml.Marshal` + atomic write.
10. ✅ **State machine defined** — `lifecycle.go` with `CanTransitionTo()` + `TransitionSite()` DB method.

---

## What Remains (Priority Order)

### P0 — Wire the state machine into execution paths

1. **DB migration** — MySQL `sites.status` column → support new enum values. Add `desired_custom_domain VARCHAR(255)`, `validated_at TIMESTAMP`, `last_reconciled_at TIMESTAMP`, `UNIQUE INDEX idx_custom_domain (custom_domain)`.
2. **Migrate `CompleteJob`/`FailJob`** — Replace raw `UpdateSiteStatus()` calls with `TransitionSite()` so the state machine is actually enforced, not just available.

### P1 — Infrastructure interfaces (testability)

3. **Define `EdgeProvider` interface** — `ApplySite(site Site) error`, `RemoveSite(name string) error`, `RegenerateAll(sites []Site) error`.
4. **Define `TunnelProvider` interface** — `RouteDomain(domain string) error`, `RemoveRoute(domain string) error`, `RegenerateConfig() error`.
5. **Implement `DockerEdgeProvider`** wrapping current nginx logic behind the interface.
6. **Wire `TunnelManager` behind `TunnelProvider`** interface.
7. **Add `TunnelManager.RegenerateAll()`** — Full config rebuild from `ListCustomDomains()` instead of incremental patching.
8. **Replace direct calls in provisioner/destroyer** with interface method calls.

### P2 — Async domain lifecycle

9. **Refactor `handleSetCustomDomain`** to record-intent-only (set `desired_custom_domain`, transition to `DOMAIN_PENDING`, return 202).
10. **Add domain lifecycle processing to worker** — `DOMAIN_PENDING` → validate DNS → `DOMAIN_VALIDATING` → apply routing → `DOMAIN_ROUTING` → confirm → `DOMAIN_ACTIVE`.
11. **Refactor `handleRemoveCustomDomain`** similarly (transition to `DOMAIN_REMOVING`, worker handles teardown).

### P3 — Reconciliation loop

12. **Implement `Reconciler` struct** — Background goroutine (separate tick from job worker, e.g. 60s).
13. **Drift detection for ACTIVE sites** — Container running? Nginx config exists? Tunnel route present?
14. **Orphan detection** — Containers/configs/routes without matching DB records.
15. **Auto-repair for safe drifts** — Restart stopped containers, regenerate missing nginx configs.
16. **Replace `health.sh`/`cleanup.sh`** functionality with reconciler + API endpoints.

### P4 — Observability + Testing

17. **Structured logging** — `slog` or `OpContext` wrapper with site/job/step/elapsed on every line.
18. **State machine unit tests** — Exhaustive `CanTransitionTo()` coverage.
19. **Rollback path tests** — Every partial failure scenario with mock providers.
20. **Idempotency tests** — Call every provider method twice, assert identical outcome.
21. **Integration tests** — Full lifecycle provision → domain → destroy with real Docker.
22. **Cloudflare IP range validation** in `ValidateDomainDNS`.

---

## Architecture Invariants (Do Not Violate)

1. **DB is the single source of truth.** Nginx, cloudflared, tunnel routes are derived artifacts.
2. **Validate → Apply Infra → Confirm → Commit DB.** Never write DB before infra succeeds.
3. **State transitions must be explicit.** All status changes go through `TransitionSite()` / `CanTransitionTo()`.
4. **Every operation must be idempotent.** Safe to retry without side effects.
5. **Generate, don't patch.** Config files should be fully regenerated from DB, not incrementally modified.
6. **No fire-and-forget.** Every infra mutation must confirm success synchronously.
7. **Rollback on partial failure.** If step N fails, compensate steps 1..N-1.
