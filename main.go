package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
)

func main() {
	cfg := LoadConfig()

	// ── Control plane DB ─────────────────────────────────────────────
	db, err := NewDB(cfg.ControlDSN)
	if err != nil {
		log.Fatalf("[main] cannot connect to control DB: %v", err)
	}
	log.Println("[main] connected to control DB")

	// ── Schema migration ────────────────────────────────────
	if err := db.MigrateSchema(); err != nil {
		log.Printf("[main] schema migration warning: %v", err)
		// non-fatal — column may already exist on older MySQL versions that
		// don't support IF NOT EXISTS on ALTER TABLE
	}

	// ── Docker client (TLS to app-01) ────────────────────────────────
	docker, err := client.NewClientWithOpts(
		client.WithHost(cfg.DockerHost),
		client.WithTLSClientConfig(
			cfg.DockerCertDir+"/ca.pem",
			cfg.DockerCertDir+"/cert.pem",
			cfg.DockerCertDir+"/key.pem",
		),
		client.WithVersion("1.44"),
	)
	if err != nil {
		log.Fatalf("[main] cannot create docker client: %v", err)
	}

	// Verify Docker reachability on startup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := docker.Info(ctx)
	if err != nil {
		log.Fatalf("[main] cannot reach docker on app-01: %v", err)
	}
	log.Printf("[main] connected to docker on app-01 (containers: %d)", info.Containers)

	// ── R2 backup client ─────────────────────────────────
	r2, r2Err := NewR2Client(cfg)
	if r2Err != nil {
		log.Printf("[main] R2 not configured (%v) — backups disabled", r2Err)
	}

	// ── Wire up components ───────────────────────────────
	tunnel := NewTunnelManager(cfg)
	provisioner := NewProvisioner(docker, cfg)
	staticProvisioner := NewStaticProvisioner(docker, cfg)
	backupper := NewBackupper(docker, cfg, r2, db)
	destroyer := NewDestroyer(docker, cfg, backupper)
	worker := NewWorker(db, provisioner, destroyer, staticProvisioner, cfg)
	go worker.Start()
	log.Println("[main] worker started")

	if r2 != nil {
		backupWorker := NewBackupWorker(backupper, cfg)
		go backupWorker.Start()
		log.Println("[main] backup worker started")
	}

	// ── HTTP API ─────────────────────────────────────────────────────
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	api := NewAPI(db, cfg, docker, tunnel, backupper)
	api.RegisterRoutes(router)

	srv := &http.Server{
		Addr:         ":" + cfg.APIPort,
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[main] API listening on :%s", cfg.APIPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[main] server error: %v", err)
		}
	}()

	// ── Graceful shutdown ────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	log.Println("[main] shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] forced shutdown: %v", err)
	}
	log.Println("[main] stopped cleanly")
}
