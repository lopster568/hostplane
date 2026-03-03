# Backup Feature — Implementation Report

> **Branch:** `feat-backup` | **Date:** 2026-03-03 | `go build` ✅

---

## Summary

Full backup feature implemented in 8 logical commits. Binary builds cleanly with zero errors after `go mod tidy`. Nothing panics on missing R2 credentials — the binary degrades gracefully.

---

## Commits

| # | Hash | Scope | Description |
|---|------|-------|-------------|
| 1 | `7cec263` | deps | `aws-sdk-go-v2` packages added to `go.mod` |
| 2 | `b7e5590` | schema | R2 config fields + `last_backup_at` DB column |
| 3 | `9d5f475` | backup.go | R2 client — streaming upload, list, lifecycle delete |
| 4 | `93c9701` | backupper.go | Core backup logic — DB + volume via ephemeral containers |
| 5 | `fdcf155` | backup_worker.go | Daily scheduler goroutine |
| 6 | `553903b` | destroyer.go | Pre-destroy safety backup gate |
| 7 | `df1374b` | api.go + main.go | HTTP routes + full startup wiring |
| 8 | `059a105` | docs | `.ai/backup_plan.md` |

---

## Files Changed / Created

### New files

| File | Purpose |
|------|---------|
| `backup.go` | `R2Client` — `Upload`, `List`, `DeleteOlderThan`, key/path helpers, `stripDockerMux`, `parseDSNCredentials` |
| `backupper.go` | `Backupper` — `BackupSite`, `BackupAll`, `BackupDatabase`, `BackupVolume`, `RestoreSite` (stub) |
| `backup_worker.go` | `BackupWorker` — 24 h ticker, immediate first run, weekly cleanup on every 7th tick |

### Modified files

| File | Changes |
|------|---------|
| `config.go` | Four R2 fields added to `Config` struct + `LoadConfig()` |
| `db.go` | `Site.LastBackupAt *time.Time`; `MigrateSchema()`; `UpdateLastBackupAt()`; updated `GetSite` + `ListSites` queries |
| `destroyer.go` | `Backupper` injected into `Destroyer`; `Run()` blocks on `BackupSite()` before any destructive step |
| `api.go` | `Backupper` on `API` struct; 3 new routes; `last_backup_at` in site status response |
| `main.go` | `MigrateSchema()` on startup; `NewR2Client` (non-fatal); `NewBackupper`; updated `NewDestroyer` + `NewAPI`; conditional `BackupWorker` goroutine |
| `go.mod` / `go.sum` | `aws-sdk-go-v2/aws`, `credentials`, `service/s3`, `feature/s3/manager` |

---

## Architecture

### Backup flow (per site)

```
BackupSite(site)
  ├── BackupDatabase(site)
  │     ephemeral mysql:8 container on app-01
  │     mysqldump stdout → Docker attach → stripDockerMux → gzip → R2 Upload
  │     key: databases/<site>/2026-03-03.sql.gz
  │
  └── BackupVolume(site)
        ephemeral alpine container on app-01 (volume mounted read-only)
        tar -czf - -C /data . → Docker attach → stripDockerMux → R2 Upload
        key: volumes/<site>/2026-03-03.tar.gz
```

No disk writes on control-01. Streams flow: Docker daemon → control-01 memory → R2 multipart upload (10 MB parts).

### Destroy flow (updated)

```
Destroyer.Run(site)
  1. BackupSite(site)  ← blocks; aborts destroy on any backup failure
  2. removeContainer(php_<site>)
  3. removeContainer(nginx_<site>)
  4. removeVolume(wp_<site>)
  5. removeCaddyConfig(site)
  6. reloadCaddy()
  7. dropDatabase(wp_<site>)
```

### Scheduler

```
BackupWorker.Start()
  → BackupAll() immediately on startup
  → time.NewTicker(24h)
     on tick: BackupAll()
     every 7th tick: DeleteOlderThan(databases/, 30), DeleteOlderThan(volumes/, 30)
```

### DB schema change

```sql
ALTER TABLE sites
ADD COLUMN IF NOT EXISTS last_backup_at DATETIME NULL DEFAULT NULL;
```

Applied idempotently via `db.MigrateSchema()` called from `main()` on every startup.

---

## API Endpoints Added

| Method | Path | Behaviour |
|--------|------|-----------|
| `POST` | `/api/sites/:site/backup` | On-demand backup, synchronous, returns `{"site","status","date"}` |
| `GET` | `/api/sites/:site/backups` | Lists all R2 objects for the site — both DB and volume entries with `size_bytes` |
| `POST` | `/api/sites/:site/restore/:date` | Phase 2 stub — returns `501` with clear message |

`GET /api/sites/:site` now includes `"last_backup_at"` (null until first backup).
`GET /api/sites` includes `LastBackupAt` via struct serialisation.

---

## .env additions required on control-01

```
R2_ACCOUNT_ID=<cloudflare-account-id>
R2_ACCESS_KEY_ID=<r2-key-id>
R2_SECRET_ACCESS_KEY=<r2-secret>
R2_BUCKET=hostplane-backups
```

If any of these are absent, the binary starts normally and logs:
```
[main] R2 not configured (...) — backups disabled
```
The `Destroyer` will block destroy jobs until R2 is configured (pre-destroy backup will always fail without credentials).

---

## R2 Bucket Layout

```
hostplane-backups/
├── databases/
│   └── <site>/
│       ├── 2026-03-03.sql.gz
│       └── 2026-03-04.sql.gz
└── volumes/
    └── <site>/
        ├── 2026-03-03.tar.gz
        └── 2026-03-04.tar.gz
```

---

## Key Design Decisions

| Decision | Rationale |
|----------|-----------|
| Container-based mysqldump | No mysqldump binary on control-01; `mysql:8` runs on app-01 which is on `wp_backend` and can reach state-01 directly |
| `stripDockerMux` | Docker `ContainerAttach` adds an 8-byte frame header to every chunk; must be stripped before piping raw bytes to gzip/R2 |
| `s3manager.Uploader` (10 MB parts) | Avoids buffering the entire backup in memory; multipart upload handles streams of any size |
| Non-fatal R2 init | `go build` + startup are never blocked by missing credentials; backup is a non-critical path except during destroy |
| Pre-destroy backup blocks | The one place where backup is critical — destroy is the point of no return. Hard block is intentional |
| `last_backup_at` on `sites` table | Single column addition via idempotent migration; gives operators visibility into backup health without a separate table |
| `BackupAll` skips DESTROYED/FAILED/CREATED | No point backing up sites with no data |

---

## Phase 2 (not implemented — stubs in place)

- `RestoreSite(site, date)` — download from R2, pipe to `mysql` container for DB, `tar -xzf` container for volume
- Exit-code checking on ephemeral containers (`ContainerWait` + status code)
- Backup status endpoint (`GET /api/backup/status`) showing last run, per-site results
- `configs/` prefix for Caddy snippet exports (low value — regenerated from DB)
