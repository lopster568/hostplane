# Backup Feature — Implementation Report

> **Branch:** `feat-backup` | **Date:** 2026-03-03 | `go build` ✅ | `go vet` ✅

---

## Summary

Full backup feature implemented and hardened across 9 implementation commits + 1 review commit. Binary builds cleanly with zero errors and zero vet warnings. A security review caught 8 bugs post-implementation — all fixed before merge.

---

## Commits

| #   | Hash      | Scope            | Description                                              |
| --- | --------- | ---------------- | -------------------------------------------------------- |
| 1   | `7cec263` | deps             | `aws-sdk-go-v2` packages added to `go.mod`               |
| 2   | `b7e5590` | schema           | R2 config fields + `last_backup_at` DB column            |
| 3   | `9d5f475` | backup.go        | R2 client — streaming upload, list, lifecycle delete     |
| 4   | `93c9701` | backupper.go     | Core backup logic — DB + volume via ephemeral containers |
| 5   | `fdcf155` | backup_worker.go | Daily scheduler goroutine                                |
| 6   | `553903b` | destroyer.go     | Pre-destroy safety backup gate                           |
| 7   | `df1374b` | api.go + main.go | HTTP routes + full startup wiring                        |
| 8   | `059a105` | docs             | `.ai/backup_plan.md`                                     |
| 9   | `dd14a45` | docs             | `report.md`                                              |
| 10  | `cf6987e` | **review**       | **Hardening — 8 bugs fixed (see Review section)**        |

---

## Files Changed / Created

### New files

| File               | Purpose                                                                                                     |
| ------------------ | ----------------------------------------------------------------------------------------------------------- |
| `backup.go`        | `R2Client` — `Upload`, `List`, `DeleteOlderThan`, key/path helpers, `stripDockerMux`, `parseDSNCredentials` |
| `backupper.go`     | `Backupper` — `BackupSite`, `BackupAll`, `BackupDatabase`, `BackupVolume`, `RestoreSite` (stub)             |
| `backup_worker.go` | `BackupWorker` — 24 h ticker, immediate first run, weekly cleanup on every 7th tick                         |

### Modified files

| File                | Changes                                                                                                                                          |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `config.go`         | Four R2 fields added to `Config` struct + `LoadConfig()`                                                                                         |
| `db.go`             | `Site.LastBackupAt *time.Time`; `MigrateSchema()`; `UpdateLastBackupAt()`; updated `GetSite` + `ListSites` queries                               |
| `destroyer.go`      | `Backupper` injected into `Destroyer`; `Run()` blocks on `BackupSite()` before any destructive step                                              |
| `api.go`            | `Backupper` on `API` struct; 3 new routes; `last_backup_at` in site status response                                                              |
| `main.go`           | `MigrateSchema()` on startup; `NewR2Client` (non-fatal); `NewBackupper`; updated `NewDestroyer` + `NewAPI`; conditional `BackupWorker` goroutine |
| `go.mod` / `go.sum` | `aws-sdk-go-v2/aws`, `credentials`, `service/s3`, `feature/s3/manager`                                                                           |

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

| Method | Path                             | Behaviour                                                                        |
| ------ | -------------------------------- | -------------------------------------------------------------------------------- |
| `POST` | `/api/sites/:site/backup`        | On-demand backup, synchronous, returns `{"site","status","date"}`                |
| `GET`  | `/api/sites/:site/backups`       | Lists all R2 objects for the site — both DB and volume entries with `size_bytes` |
| `POST` | `/api/sites/:site/restore/:date` | Phase 2 stub — returns `501` with clear message                                  |

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

| Decision                                   | Rationale                                                                                                                |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------ |
| Container-based mysqldump                  | No mysqldump binary on control-01; `mysql:8` runs on app-01 which is on `wp_backend` and can reach state-01 directly     |
| `stripDockerMux`                           | Docker `ContainerAttach` adds an 8-byte frame header to every chunk; must be stripped before piping raw bytes to gzip/R2 |
| `s3manager.Uploader` (10 MB parts)         | Avoids buffering the entire backup in memory; multipart upload handles streams of any size                               |
| Non-fatal R2 init                          | `go build` + startup are never blocked by missing credentials; backup is a non-critical path except during destroy       |
| Pre-destroy backup blocks                  | The one place where backup is critical — destroy is the point of no return. Hard block is intentional                    |
| `last_backup_at` on `sites` table          | Single column addition via idempotent migration; gives operators visibility into backup health without a separate table  |
| `BackupAll` skips DESTROYED/FAILED/CREATED | No point backing up sites with no data                                                                                   |

---

## Phase 2 (not implemented — stubs in place)

- `RestoreSite(site, date)` — download from R2, pipe to `mysql` container for DB, `tar -xzf` container for volume
- Backup status endpoint (`GET /api/backup/status`) showing last run time and per-site results
- `configs/` prefix for Caddy snippet exports (low value — regenerated from DB)

---

## Review — Bugs Found and Fixed (commit `cf6987e`)

### Bug 1 — CRITICAL: No container exit-code check (BackupDatabase + BackupVolume)

**Problem:** `mysqldump` or `tar` could fail (wrong credentials, DB not found, volume error) and exit with code 1. Stdout would be empty or partial. The upload would succeed from R2's perspective — a 0-byte or partial `.sql.gz` would be stored. `last_backup_at` would be stamped. The pre-destroy backup gate would give a false green light on corrupt data.

**Fix:** Added `ContainerWait` after upload completes. If exit code ≠ 0:

1. Call `logContainerStderr` to capture and log the error message from the container
2. If the upload had succeeded (i.e., an object exists in R2), call `r2.deleteObject` to remove the corrupt object
3. Return an error

### Bug 2 — CRITICAL: Corrupt object left in R2 on process failure

**Problem:** Even if we detected exit code ≠ 0, the bad object was already persisted in R2 with a valid-looking key (`2026-03-03.sql.gz`). Any restore attempt would use this corrupt file.

**Fix:** `deleteObject` helper added to `R2Client`. Called by `BackupDatabase` and `BackupVolume` when exit code ≠ 0 and the upload had already succeeded. Failure to delete is logged but does not mask the original error.

### Bug 3 — `stripDockerMux` passed `io.EOF` to `pw.CloseWithError`

**Problem:** When the container stdout stream closes cleanly, `io.ReadFull` returns `io.EOF`. Passing this to `pw.CloseWithError(io.EOF)` conflated a normal end-of-stream with an error condition. While `io.Copy` does handle `io.EOF` from readers as a clean termination, the intent was wrong and could cause subtle issues if the pipe reader was used in other ways.

**Fix:** Explicit `if err == io.EOF { pw.Close(); return }` — using `pw.Close()` (clean close) instead of `CloseWithError` for the normal termination path.

### Bug 4 — `List` not paginated (silent data loss on >1000 objects)

**Problem:** `ListObjectsV2` returns at most 1000 objects per call. With no pagination, `DeleteOlderThan` and `handleListBackups` would silently ignore all objects beyond the first page. Long-lived buckets with many sites would accumulate objects that are never cleaned up.

**Fix:** `List` now follows `NextContinuationToken` in a loop until `IsTruncated == false`.

### Bug 5 — `DeleteOlderThan` aborted on first delete failure

**Problem:** A single failed object deletion (e.g., permissions error) would return an error and stop processing, leaving all subsequent old objects undeleted.

**Fix:** Log the error and `continue` the loop. Return the last error seen after all objects have been processed.

### Bug 6 — `handleListBackups` nil pointer panic when R2 unconfigured

**Problem:** `handleListBackups` called `a.backupper.r2.List(...)` directly. If R2 credentials were absent, `a.backupper.r2` is `nil` — a guaranteed nil pointer panic on any request to `GET /sites/:site/backups`.

**Fix:** Explicit `if a.backupper.r2 == nil` guard at the top of the handler; returns `503 Service Unavailable` with a clear message.

### Bug 7 — No stderr capture for failed backup containers

**Problem:** When `mysqldump` or `tar` failed, the error message (e.g., "Access denied for user", "Cannot find volume") went to stderr which was silently discarded. Operators saw only "exited with code 1" with no actionable information.

**Fix:** Added `logContainerStderr` helper that calls `ContainerLogs` with `ShowStderr: true` after the container exits and logs up to 4 KB. Note: cannot reuse `stripDockerMux` for this because that function filters to stream type 1 (stdout); stderr frames are type 2 and would be discarded. `logContainerStderr` reads the mux frame headers directly.

### Bug 8 — `--skip-lock-tables` conflicts with `--single-transaction`

**Problem:** `--single-transaction` takes a consistent InnoDB snapshot at the start of the dump — no locks needed. `--skip-lock-tables` disables locking for non-transactional tables (MyISAM). Having both is contradictory and `--skip-lock-tables` can produce inconsistent results if any non-InnoDB tables exist. WordPress is all InnoDB.

**Fix:** Removed `--skip-lock-tables`. Added `--set-gtid-purged=OFF` to avoid GTID-related warnings on setups without replication.
