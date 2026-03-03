package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// Backupper handles site backups to R2. It mirrors the Provisioner pattern:
// same Docker client, same Config, same naming helpers.
type Backupper struct {
	docker *client.Client
	cfg    Config
	r2     *R2Client // may be nil when R2 is not configured
	db     *DB       // control-plane DB — for ListSites and UpdateLastBackupAt
}

func NewBackupper(docker *client.Client, cfg Config, r2 *R2Client, db *DB) *Backupper {
	return &Backupper{docker: docker, cfg: cfg, r2: r2, db: db}
}

// BackupSite backs up the database and volume for a single site.
// Both must succeed — the first failure aborts and returns an error.
// Called by Destroyer.Run() before any destructive step.
func (b *Backupper) BackupSite(site string) error {
	if b.r2 == nil {
		return fmt.Errorf("R2 not configured — cannot back up site %s", site)
	}
	if err := b.BackupDatabase(site); err != nil {
		return fmt.Errorf("database backup failed: %w", err)
	}
	if err := b.BackupVolume(site); err != nil {
		return fmt.Errorf("volume backup failed: %w", err)
	}

	// Record successful backup time in the control DB
	if err := b.db.UpdateLastBackupAt(site, time.Now().UTC()); err != nil {
		log.Printf("[backupper] site=%s warning: could not update last_backup_at: %v", site, err)
		// non-fatal — the backup data is in R2 regardless
	}

	log.Printf("[backupper] site=%s backup complete (db + volume)", site)
	return nil
}

// BackupAll backs up every known active site. A single site failure is logged
// but never stops other sites from being backed up.
func (b *Backupper) BackupAll() {
	sites, err := b.db.ListSites()
	if err != nil {
		log.Printf("[backupper] BackupAll: cannot list sites: %v", err)
		return
	}

	ok, failed := 0, 0
	for _, s := range sites {
		// Only back up sites that actually have data
		if s.Status == "DESTROYED" || s.Status == "FAILED" || s.Status == "CREATED" {
			continue
		}
		if err := b.BackupSite(s.Site); err != nil {
			log.Printf("[backupper] BackupAll: site=%s FAILED: %v", s.Site, err)
			failed++
			continue // never stops the loop
		}
		ok++
	}
	log.Printf("[backupper] BackupAll complete: %d ok, %d failed", ok, failed)
}

// BackupDatabase runs mysqldump on state-01 for the site's database and streams
// the gzip-compressed output directly to R2. No disk writes on control-01.
//
// Mechanism: spawns a short-lived mysql:8 container on app-01 (which is already
// on the wp_backend network and can reach state-01). Stdout is piped via Docker
// ContainerAttach → stripDockerMux → gzip → R2.
func (b *Backupper) BackupDatabase(site string) error {
	if b.r2 == nil {
		return fmt.Errorf("R2 not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dbName := WPDatabaseName(site)
	dbUser, dbPass := parseDSNCredentials(b.cfg.WordPressDSN)
	dbHostPort := b.cfg.DBHost() // e.g. "10.10.0.20:3306"
	dbHost, dbPort, err := net.SplitHostPort(dbHostPort)
	if err != nil {
		dbHost = dbHostPort
		dbPort = "3306"
	}

	containerName := fmt.Sprintf("backup_db_%s_%d", site, time.Now().UnixNano())

	createResp, err := b.docker.ContainerCreate(ctx,
		&container.Config{
			Image: "mysql:8",
			Cmd: []string{
				"mysqldump",
				"-h", dbHost,
				"-P", dbPort,
				"-u", dbUser,
				fmt.Sprintf("-p%s", dbPass),
				"--single-transaction",
				"--quick",
				"--skip-lock-tables",
				dbName,
			},
			AttachStdout: true,
			AttachStderr: true,
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(b.cfg.DockerNetwork),
			AutoRemove:  false, // we remove manually after cleanup
		},
		nil, nil, containerName,
	)
	if err != nil {
		return fmt.Errorf("create mysqldump container: %w", err)
	}

	// Ensure cleanup regardless of outcome
	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		b.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true})
	}()

	// Attach BEFORE start to capture all output from the beginning
	attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: false,
	})
	if err != nil {
		return fmt.Errorf("attach mysqldump container: %w", err)
	}
	defer attachResp.Close()

	if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start mysqldump container: %w", err)
	}

	// Strip Docker mux headers to get raw stdout SQL text
	rawStream := stripDockerMux(attachResp.Reader)

	// Gzip on-the-fly: rawStream → gzip.Writer → pr (pipe reader sent to R2)
	pr, pw := io.Pipe()
	gzDone := make(chan error, 1)
	go func() {
		gz := gzip.NewWriter(pw)
		_, copyErr := io.Copy(gz, rawStream)
		gz.Close()
		pw.CloseWithError(copyErr)
		gzDone <- copyErr
	}()

	key := keyForDB(site, dateStamp())
	uploadErr := b.r2.Upload(ctx, key, pr, "application/gzip")

	// Wait for gzip goroutine to finish
	<-gzDone

	if uploadErr != nil {
		return fmt.Errorf("upload DB backup to R2: %w", uploadErr)
	}

	log.Printf("[backupper] site=%s DB backup → %s", site, key)
	return nil
}

// BackupVolume creates a tar.gz of the wp_<site> Docker volume and streams it
// directly to R2. No disk writes on control-01.
//
// Mechanism: spawns a short-lived alpine container on app-01 with the site
// volume mounted read-only. Runs: tar -czf - -C /data . so stdout is the
// complete tar.gz. Streamed via ContainerAttach → stripDockerMux → R2.
func (b *Backupper) BackupVolume(site string) error {
	if b.r2 == nil {
		return fmt.Errorf("R2 not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	volumeName := VolumeName(site)
	containerName := fmt.Sprintf("backup_vol_%s_%d", site, time.Now().UnixNano())

	createResp, err := b.docker.ContainerCreate(ctx,
		&container.Config{
			Image:        "alpine:latest",
			Cmd:          []string{"tar", "-czf", "-", "-C", "/data", "."},
			AttachStdout: true,
			AttachStderr: false,
		},
		&container.HostConfig{
			AutoRemove: false,
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

	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		b.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true})
	}()

	attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: false,
	})
	if err != nil {
		return fmt.Errorf("attach volume backup container: %w", err)
	}
	defer attachResp.Close()

	if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start volume backup container: %w", err)
	}

	// tar -czf already produces gzip output — stream directly to R2
	rawStream := stripDockerMux(attachResp.Reader)

	key := keyForVolume(site, dateStamp())
	if err := b.r2.Upload(ctx, key, rawStream, "application/x-tar"); err != nil {
		return fmt.Errorf("upload volume backup to R2: %w", err)
	}

	log.Printf("[backupper] site=%s volume backup → %s", site, key)
	return nil
}

// RestoreSite downloads and restores backups for the given date (YYYY-MM-DD).
// Phase 2 — not yet implemented.
func (b *Backupper) RestoreSite(site, date string) error {
	return fmt.Errorf("restore not yet implemented (phase 2)")
}
