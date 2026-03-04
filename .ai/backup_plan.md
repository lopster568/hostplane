# Backup Feature — Implementable Plan

> **Date: 2026-03-03** | Based on current codebase audit

---

## Dependency

Add AWS SDK v2 to `go.mod`. Only three sub-packages needed:

```bash
go get github.com/aws/aws-sdk-go-v2/aws
go get github.com/aws/aws-sdk-go-v2/credentials
go get github.com/aws/aws-sdk-go-v2/service/s3
```

R2 is S3-compatible — no other library needed.

---

## Step 1 — `config.go`

Add four fields to the existing `Config` struct and load them in `LoadConfig()`.

### Struct additions (after `StuckJobTimeout int`):

```go
// Backup (R2)
R2AccountID       string
R2AccessKeyID     string
R2SecretAccessKey string
R2Bucket          string
```

### `LoadConfig()` additions (use `getEnv` — same as every other field):

```go
R2AccountID:       getEnv("R2_ACCOUNT_ID", ""),
R2AccessKeyID:     getEnv("R2_ACCESS_KEY_ID", ""),
R2SecretAccessKey: getEnv("R2_SECRET_ACCESS_KEY", ""),
R2Bucket:          getEnv("R2_BUCKET", "hostplane-backups"),
```

No `mustEnv` — the binary should start fine without R2 configured; backup ops
will fail gracefully at call time rather than crashing startup.

---

## Step 2 — `backup.go` (R2 client)

Single struct wrapping the S3 client. Three methods: `Upload`, `List`, `DeleteOlderThan`.

```go
package main

import (
    "context"
    "fmt"
    "io"
    "strings"
    "time"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/credentials"
    "github.com/aws/aws-sdk-go-v2/service/s3"
)

type R2Client struct {
    s3     *s3.Client
    bucket string
}

func NewR2Client(cfg Config) (*R2Client, error) {
    if cfg.R2AccountID == "" || cfg.R2AccessKeyID == "" || cfg.R2SecretAccessKey == "" {
        return nil, fmt.Errorf("R2 credentials not configured")
    }

    endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.R2AccountID)

    s3Client := s3.NewFromConfig(aws.Config{
        Region: "auto",
        Credentials: credentials.NewStaticCredentialsProvider(
            cfg.R2AccessKeyID,
            cfg.R2SecretAccessKey,
            "",
        ),
        EndpointResolverWithOptions: aws.EndpointResolverWithOptionsFunc(
            func(service, region string, options ...interface{}) (aws.Endpoint, error) {
                return aws.Endpoint{URL: endpoint}, nil
            },
        ),
    })

    return &R2Client{s3: s3Client, bucket: cfg.R2Bucket}, nil
}

// Upload streams reader to R2 at the given key.
// Caller is responsible for closing the reader.
func (r *R2Client) Upload(ctx context.Context, key string, reader io.Reader, contentType string) error {
    _, err := r.s3.PutObject(ctx, &s3.PutObjectInput{
        Bucket:      aws.String(r.bucket),
        Key:         aws.String(key),
        Body:        reader,
        ContentType: aws.String(contentType),
    })
    return err
}

// BackupEntry holds metadata for a single backup object in R2.
type BackupEntry struct {
    Key          string
    LastModified time.Time
    Size         int64
}

// List returns all backup entries matching the given prefix (e.g. "databases/mysite/").
func (r *R2Client) List(ctx context.Context, prefix string) ([]BackupEntry, error) {
    resp, err := r.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
        Bucket: aws.String(r.bucket),
        Prefix: aws.String(prefix),
    })
    if err != nil {
        return nil, err
    }

    entries := make([]BackupEntry, 0, len(resp.Contents))
    for _, obj := range resp.Contents {
        entries = append(entries, BackupEntry{
            Key:          aws.ToString(obj.Key),
            LastModified: aws.ToTime(obj.LastModified),
            Size:         aws.ToInt64(obj.Size),
        })
    }
    return entries, nil
}

// DeleteOlderThan removes all objects under prefix older than `days` days.
// Used for lifecycle cleanup — call weekly from the backup worker.
func (r *R2Client) DeleteOlderThan(ctx context.Context, prefix string, days int) error {
    entries, err := r.List(ctx, prefix)
    if err != nil {
        return err
    }

    cutoff := time.Now().AddDate(0, 0, -days)
    for _, e := range entries {
        if e.LastModified.Before(cutoff) {
            if _, err := r.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
                Bucket: aws.String(r.bucket),
                Key:    aws.String(e.Key),
            }); err != nil {
                return fmt.Errorf("delete %s: %w", e.Key, err)
            }
        }
    }
    return nil
}

// keyForDB returns the R2 object key for a database backup.
// e.g. databases/mysite/2026-03-03.sql.gz
func keyForDB(site, date string) string {
    return fmt.Sprintf("databases/%s/%s.sql.gz", site, date)
}

// keyForVolume returns the R2 object key for a volume backup.
// e.g. volumes/mysite/2026-03-03.tar.gz
func keyForVolume(site, date string) string {
    return fmt.Sprintf("volumes/%s/%s.tar.gz", site, date)
}

// dateStamp returns today's date as YYYY-MM-DD for use in object keys.
func dateStamp() string {
    return time.Now().UTC().Format("2006-01-02")
}

// stripDockerMux strips the 8-byte Docker multiplexing header from each frame
// in an io.Reader, returning a plain byte stream suitable for piping to R2.
// Docker ContainerLogs output is multiplexed: [stream_type(1) pad(3) size(4) data...].
// We need raw stdout bytes only.
func stripDockerMux(r io.Reader) io.Reader {
    pr, pw := io.Pipe()
    go func() {
        header := make([]byte, 8)
        for {
            if _, err := io.ReadFull(r, header); err != nil {
                pw.CloseWithError(err)
                return
            }
            // header[0]: 1 = stdout, 2 = stderr
            size := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
            if header[0] == 1 { // stdout only
                if _, err := io.CopyN(pw, r, int64(size)); err != nil {
                    pw.CloseWithError(err)
                    return
                }
            } else {
                // skip stderr frame
                if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
                    pw.CloseWithError(err)
                    return
                }
            }
        }
    }()
    return pr
}

// prefixForSiteDB returns the R2 prefix to list all DB backups for a site.
func prefixForSiteDB(site string) string {
    return fmt.Sprintf("databases/%s/", site)
}

// prefixForSiteVolume returns the R2 prefix to list all volume backups for a site.
func prefixForSiteVolume(site string) string {
    return fmt.Sprintf("volumes/%s/", site)
}

// dateFromKey extracts the YYYY-MM-DD portion from an R2 object key.
// e.g. "databases/mysite/2026-03-03.sql.gz" → "2026-03-03"
func dateFromKey(key string) string {
    parts := strings.Split(key, "/")
    if len(parts) == 0 {
        return ""
    }
    name := parts[len(parts)-1]
    // strip extension(s): .sql.gz or .tar.gz
    name = strings.TrimSuffix(name, ".sql.gz")
    name = strings.TrimSuffix(name, ".tar.gz")
    return name
}
```

---

## Step 3 — `backupper.go`

Mirrors `provisioner.go` in struct shape. Uses the existing Docker client — no
new client created. Database access mirrors the pattern in `provisioner.go`'s
`createDatabase()` — `sql.Open` → `db.Ping()` → exec.

```go
package main

import (
    "context"
    "fmt"
    "io"
    "log"
    "time"

    "github.com/docker/docker/api/types"
    "github.com/docker/docker/api/types/container"
    "github.com/docker/docker/api/types/mount"
    "github.com/docker/docker/client"
)

type Backupper struct {
    docker *client.Client
    cfg    Config
    r2     *R2Client
    db     *DB // control-plane DB — for ListSites()
}

func NewBackupper(docker *client.Client, cfg Config, r2 *R2Client, db *DB) *Backupper {
    return &Backupper{docker: docker, cfg: cfg, r2: r2, db: db}
}

// BackupSite backs up both the MariaDB database and the Docker volume for a
// site. Both must succeed — if either fails the error is returned immediately.
// This is the function called from Destroyer.Run() before any destructive steps.
func (b *Backupper) BackupSite(site string) error {
    if err := b.BackupDatabase(site); err != nil {
        return fmt.Errorf("database backup: %w", err)
    }
    if err := b.BackupVolume(site); err != nil {
        return fmt.Errorf("volume backup: %w", err)
    }
    log.Printf("[backupper] site=%s backup complete", site)
    return nil
}

// BackupAll backs up every known site. Single-site failures are logged but do
// not abort the loop — all sites are attempted regardless.
func (b *Backupper) BackupAll() {
    sites, err := b.db.ListSites()
    if err != nil {
        log.Printf("[backupper] BackupAll: cannot list sites: %v", err)
        return
    }

    for _, s := range sites {
        if err := b.BackupSite(s.Site); err != nil {
            log.Printf("[backupper] BackupAll: site=%s FAILED: %v", s.Site, err)
            // continue — never stop other sites from being backed up
            continue
        }
    }
}

// BackupDatabase runs mysqldump against state-01 for the site's database,
// streams the output gzipped directly to R2.
//
// Implementation: spawns a short-lived mysql:8 container on app-01 (which is
// already on the wp_backend network and can reach state-01 at 10.10.0.20).
// Container runs: mysqldump --single-transaction --quick <dbname>
// stdout is piped through the Docker log stream → gzip → R2.
//
// No temp files on disk. No mysqldump binary required on control-01.
func (b *Backupper) BackupDatabase(site string) error {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
    defer cancel()

    dbName := WPDatabaseName(site)
    dbHost := b.cfg.DBHost() // "10.10.0.20:3306" from WordPressDSN
    // Extract DSN credentials for the WP root user
    // WordPressDSN format: user:pass@tcp(host:port)/
    user, pass := parseDSNCredentials(b.cfg.WordPressDSN)

    containerName := fmt.Sprintf("backup_db_%s_%d", site, time.Now().Unix())

    // Create the one-shot mysqldump container
    createResp, err := b.docker.ContainerCreate(ctx,
        &container.Config{
            Image: "mysql:8",
            Cmd: []string{
                "mysqldump",
                "-h", dbHost[:len(dbHost)-5], // strip :3306 port for -h flag
                "-P", "3306",
                "-u", user,
                fmt.Sprintf("-p%s", pass),
                "--single-transaction",
                "--quick",
                dbName,
            },
            AttachStdout: true,
            AttachStderr: true,
        },
        &container.HostConfig{
            NetworkMode: container.NetworkMode(b.cfg.DockerNetwork),
            AutoRemove:  true,
        },
        nil, nil, containerName,
    )
    if err != nil {
        return fmt.Errorf("create mysqldump container: %w", err)
    }

    // Attach before start so we capture all output from the beginning
    attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
        Stream: true,
        Stdout: true,
        Stderr: false,
        Logs:   false,
    })
    if err != nil {
        return fmt.Errorf("attach to mysqldump container: %w", err)
    }
    defer attachResp.Close()

    if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
        return fmt.Errorf("start mysqldump container: %w", err)
    }

    // The attach reader uses Docker's multiplexing format — strip headers
    rawStream := stripDockerMux(attachResp.Reader)

    // Gzip on-the-fly before upload
    gzReader, gzWriter := io.Pipe()
    go func() {
        gz := gzip.NewWriter(gzWriter)
        _, copyErr := io.Copy(gz, rawStream)
        gz.Close()
        gzWriter.CloseWithError(copyErr)
    }()

    key := keyForDB(site, dateStamp())
    if err := b.r2.Upload(ctx, key, gzReader, "application/gzip"); err != nil {
        return fmt.Errorf("upload to R2: %w", err)
    }

    log.Printf("[backupper] site=%s DB backup → %s", site, key)
    return nil
}

// BackupVolume creates a tar.gz of the wp_<site> Docker volume and streams it
// directly to R2.
//
// Implementation: spawns a short-lived alpine container on app-01 with the
// volume mounted read-only at /data. Runs: tar -czf - -C /data .
// stdout is the tar stream, piped directly to R2.
// Uses AutoRemove — container is cleaned up by the Docker daemon on exit.
func (b *Backupper) BackupVolume(site string) error {
    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
    defer cancel()

    volumeName := VolumeName(site) // wp_<site>
    containerName := fmt.Sprintf("backup_vol_%s_%d", site, time.Now().Unix())

    createResp, err := b.docker.ContainerCreate(ctx,
        &container.Config{
            Image:        "alpine:latest",
            Cmd:          []string{"tar", "-czf", "-", "-C", "/data", "."},
            AttachStdout: true,
            AttachStderr: false,
        },
        &container.HostConfig{
            AutoRemove: true,
            Mounts: []mount.Mount{
                {
                    Type:     mount.TypeVolume,
                    Source:   volumeName,
                    Target:   "/data",
                    ReadOnly: true,
                },
            },
        },
        nil, nil, containerName,
    )
    if err != nil {
        return fmt.Errorf("create volume backup container: %w", err)
    }

    attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
        Stream: true,
        Stdout: true,
        Stderr: false,
        Logs:   false,
    })
    if err != nil {
        return fmt.Errorf("attach to volume backup container: %w", err)
    }
    defer attachResp.Close()

    if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
        return fmt.Errorf("start volume backup container: %w", err)
    }

    // Docker mux header stripping — get raw stdout tar.gz bytes
    rawStream := stripDockerMux(attachResp.Reader)

    key := keyForVolume(site, dateStamp())
    if err := b.r2.Upload(ctx, key, rawStream, "application/x-tar"); err != nil {
        return fmt.Errorf("upload to R2: %w", err)
    }

    log.Printf("[backupper] site=%s volume backup → %s", site, key)
    return nil
}

// RestoreSite downloads the database and volume backups for `date` and restores them.
// `date` must be in YYYY-MM-DD format.
//
// Database restore: pulls the .sql.gz from R2 → spawns a mysql:8 container
// with stdin piped from the decompressed stream.
// Volume restore: pulls the .tar.gz from R2 → spawns an alpine container with
// the volume mounted, pipes tar -xzf - -C /data to restore.
//
// WARNING: destructive — overwrites existing data. Call only after confirming
// the site is stopped or non-existent.
func (b *Backupper) RestoreSite(site, date string) error {
    // Restore is deferred to Phase 2 — stub here for API completeness.
    // Implementation follows the same container-attach pattern in reverse.
    return fmt.Errorf("restore not yet implemented")
}

// parseDSNCredentials extracts user and password from a DSN of the form:
// user:pass@tcp(host:port)/
func parseDSNCredentials(dsn string) (user, pass string) {
    at := len(dsn) - len(dsn[strings.Index(dsn, "@"):])
    creds := dsn[:at]
    colon := strings.Index(creds, ":")
    if colon < 0 {
        return creds, ""
    }
    return creds[:colon], creds[colon+1:]
}
```

> **Note on `parseDSNCredentials`**: add `"strings"` to the import block.  
> **Note on gzip**: add `"compress/gzip"` to the import block.  
> **Note on `DBHost()` port stripping**: the `DBHost()` helper returns `host:port`.
> For the `-h` flag we need only the host. Use:
>
> ```go
> host, _, _ := net.SplitHostPort(b.cfg.DBHost())
> ```
>
> Add `"net"` to imports.

---

## Step 4 — `destroyer.go`

Add a single `BackupSite` call at the **very top** of `Destroyer.Run()`, before
any destructive steps. The `Destroyer` struct needs to gain a `*Backupper` field.

### Struct change:

```go
type Destroyer struct {
    docker    *client.Client
    cfg       Config
    backupper *Backupper   // ← add this
}

func NewDestroyer(docker *client.Client, cfg Config, backupper *Backupper) *Destroyer {
    return &Destroyer{docker: docker, cfg: cfg, backupper: backupper}
}
```

### `Run()` change — add these lines before `removeContainer(phpName)`:

```go
func (d *Destroyer) Run(site string) error {
    // Pre-destroy safety backup — must succeed before any data is deleted.
    if err := d.backupper.BackupSite(site); err != nil {
        return fmt.Errorf("pre-destroy backup failed, aborting destroy: %w", err)
    }

    dbName := WPDatabaseName(site)
    // ... rest unchanged
```

### `main.go` — update `NewDestroyer` call:

```go
// After backupper is created:
backupper := NewBackupper(docker, cfg, r2, db)
destroyer := NewDestroyer(docker, cfg, backupper)
```

---

## Step 5 — `backup_worker.go`

Mirrors `worker.go` exactly: a struct, a constructor, a `Start()` method with
`time.NewTicker`.

```go
package main

import (
    "log"
    "time"
)

type BackupWorker struct {
    backupper *Backupper
    cfg       Config
}

func NewBackupWorker(backupper *Backupper, cfg Config) *BackupWorker {
    return &BackupWorker{backupper: backupper, cfg: cfg}
}

func (bw *BackupWorker) Start() {
    log.Println("[backup-worker] starting — daily interval")

    // Run an immediate backup on startup so the first run doesn't wait 24 h.
    // Remove this if startup noise is not acceptable.
    bw.backupper.BackupAll()

    ticker := time.NewTicker(24 * time.Hour)
    defer ticker.Stop()

    day := 0
    for range ticker.C {
        log.Println("[backup-worker] daily backup run starting")
        bw.backupper.BackupAll()
        day++

        // Weekly cleanup — every 7 ticks
        if day%7 == 0 {
            if err := bw.backupper.r2.DeleteOlderThan(context.Background(), "databases/", 30); err != nil {
                log.Printf("[backup-worker] cleanup databases/: %v", err)
            }
            if err := bw.backupper.r2.DeleteOlderThan(context.Background(), "volumes/", 30); err != nil {
                log.Printf("[backup-worker] cleanup volumes/: %v", err)
            }
        }
    }
}
```

> Add `"context"` to imports.

---

## Step 6 — `api.go`

### Add `backupper *Backupper` to the `API` struct and constructor:

```go
type API struct {
    db        *DB
    cfg       Config
    docker    *client.Client
    tunnel    *TunnelManager
    backupper *Backupper   // ← add
}

func NewAPI(db *DB, cfg Config, docker *client.Client, tunnel *TunnelManager, backupper *Backupper) *API {
    return &API{db: db, cfg: cfg, docker: docker, tunnel: tunnel, backupper: backupper}
}
```

Update the call in `main.go`:

```go
api := NewAPI(db, cfg, docker, tunnel, backupper)
```

### Add four routes in `RegisterRoutes`:

```go
v1.POST("/sites/:site/backup",          a.handleBackupSite)
v1.GET("/sites/:site/backups",          a.handleListBackups)
v1.POST("/sites/:site/restore/:date",   a.handleRestoreSite)
```

### Handler implementations:

```go
// POST /api/sites/:site/backup
// Triggers an on-demand backup. Runs synchronously and returns when done.
func (a *API) handleBackupSite(c *gin.Context) {
    site := c.Param("site")
    if _, err := a.db.GetSite(site); err != nil {
        c.JSON(http.StatusNotFound, gin.H{"error": "site not found"})
        return
    }
    if err := a.backupper.BackupSite(site); err != nil {
        log.Printf("[api] backup site=%s failed: %v", site, err)
        c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
        return
    }
    c.JSON(http.StatusOK, gin.H{"site": site, "status": "backed_up", "date": dateStamp()})
}

// GET /api/sites/:site/backups
// Returns a list of available backup dates for the site,
// including both database and volume entries.
func (a *API) handleListBackups(c *gin.Context) {
    site := c.Param("site")
    ctx := c.Request.Context()

    dbBackups, err := a.backupper.r2.List(ctx, prefixForSiteDB(site))
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot list DB backups: " + err.Error()})
        return
    }
    volBackups, err := a.backupper.r2.List(ctx, prefixForSiteVolume(site))
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot list volume backups: " + err.Error()})
        return
    }

    type backupItem struct {
        Date string `json:"date"`
        Type string `json:"type"`
        Key  string `json:"key"`
        Size int64  `json:"size_bytes"`
    }
    var items []backupItem
    for _, e := range dbBackups {
        items = append(items, backupItem{Date: dateFromKey(e.Key), Type: "database", Key: e.Key, Size: e.Size})
    }
    for _, e := range volBackups {
        items = append(items, backupItem{Date: dateFromKey(e.Key), Type: "volume", Key: e.Key, Size: e.Size})
    }

    c.JSON(http.StatusOK, gin.H{"site": site, "backups": items})
}

// POST /api/sites/:site/restore/:date
// Restores both the database and volume from the specified date (YYYY-MM-DD).
// Site should be FAILED or DESTROYED before calling this.
func (a *API) handleRestoreSite(c *gin.Context) {
    site := c.Param("site")
    date := c.Param("date")

    if err := a.backupper.RestoreSite(site, date); err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
        return
    }
    c.JSON(http.StatusOK, gin.H{"site": site, "date": date, "status": "restored"})
}
```

---

## Step 7 — `main.go`

Full wiring block. Changes are minimal — add R2 init, backupper, backup worker:

```go
// After docker client is verified reachable:

// ── R2 backup client ─────────────────────────────────────────────
r2, err := NewR2Client(cfg)
if err != nil {
    log.Printf("[main] R2 not configured (%v) — backups disabled", err)
    // r2 remains nil; Backupper will fail gracefully on first call
}

// ── Wire up components ───────────────────────────────────────────
tunnel            := NewTunnelManager(cfg)
provisioner       := NewProvisioner(docker, cfg)
staticProvisioner := NewStaticProvisioner(docker, cfg)
backupper         := NewBackupper(docker, cfg, r2, db)  // r2 may be nil
destroyer         := NewDestroyer(docker, cfg, backupper)
worker            := NewWorker(db, provisioner, destroyer, staticProvisioner, cfg)

go worker.Start()
log.Println("[main] worker started")

if r2 != nil {
    backupWorker := NewBackupWorker(backupper, cfg)
    go backupWorker.Start()
    log.Println("[main] backup worker started")
}
```

> When `r2 == nil`, `Backupper` methods still compile. `NewR2Client` is called
> in the backupper constructor — move the nil guard there so every method returns
> `fmt.Errorf("R2 not configured")` cleanly without a nil pointer panic.
> Concretely: store `r2 *R2Client` on `Backupper` and add this guard at the top
> of `BackupDatabase` and `BackupVolume`:
>
> ```go
> if b.r2 == nil {
>     return fmt.Errorf("R2 client not configured")
> }
> ```

---

## .env additions (control-01)

```
R2_ACCOUNT_ID=
R2_ACCESS_KEY_ID=
R2_SECRET_ACCESS_KEY=
R2_BUCKET=hostplane-backups
```

---

## R2 Bucket Structure

```
hostplane-backups/
├── databases/
│   └── <site>/
│       └── 2026-03-03.sql.gz
└── volumes/
    └── <site>/
        └── 2026-03-03.tar.gz
```

The `configs/` prefix from the original plan is omitted — Caddy config files are
regenerated deterministically from the DB; backing up small text files adds
complexity with near-zero restore value.

---

## Docker Hub Image Availability Check

Before implementing, verify these images are pullable on app-01:

```bash
docker pull mysql:8      # for mysqldump container
docker pull alpine:latest # for volume tar container
```

Both are standard official images. The `mysql:8` image is ~600 MB; if image
pull time on first backup run is a concern, add a pre-pull step to provisioning.

---

## Known Risks and Mitigations

| Risk                                             | Mitigation                                                                                   |
| ------------------------------------------------ | -------------------------------------------------------------------------------------------- |
| `mysql:8` image pull blocks first backup         | Pre-pull in `main.go` startup or add to provisioner                                          |
| Container not auto-removed on crash              | Use `docker rm -f backup_db_<site>_*` in `cleanup.sh`                                        |
| Volume backup races with live writes             | WordPress writes are generally safe to snapshot mid-write for disaster recovery purposes     |
| R2 upload timeout for large volumes              | 15-minute context timeout should be sufficient; increase for > 5 GB volumes                  |
| `mysqldump` password in container env            | Passed as `-pPASS` CLI arg — visible in `docker inspect`. Acceptable for LXC-contained infra |
| Pre-destroy backup adds latency to `destroy` job | Expected — this is intentional. Operator must accept longer destroy time                     |

---

## Implementation Order

1. `config.go` — add 4 fields (2 min)
2. `backup.go` — R2 client + helpers (30 min)
3. `backupper.go` — `BackupDatabase` + `BackupVolume` + `BackupAll` (1–2 hr)
4. `destroyer.go` — inject `Backupper`, add pre-destroy call (10 min)
5. `main.go` — wire R2 + backupper + backup worker (15 min)
6. `backup_worker.go` — scheduler (15 min)
7. `api.go` — 3 routes + handlers (30 min)

Total estimated: ~3–4 hours including testing each step manually.
