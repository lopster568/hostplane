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

	// ── Wire up components ───────────────────────────────────────────
	provisioner := NewProvisioner(docker, cfg)
	staticProvisioner := NewStaticProvisioner(docker, cfg)
	destroyer   := NewDestroyer(docker, cfg)
	worker := NewWorker(db, provisioner, destroyer, staticProvisioner, cfg)	
go worker.Start()
	log.Println("[main] worker started")

	// ── HTTP API ─────────────────────────────────────────────────────
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	api := NewAPI(db, cfg)
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
