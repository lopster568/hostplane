package main

import (
	"log"
	"os"
	"strings"
)

type Config struct {
	// API
	APIPort string
	APIKey  string

	// Databases
	ControlDSN   string // controlplane DB (jobs, sites)
	WordPressDSN string // root-level DSN to create wp_ databases

	// Docker
	DockerHost    string
	DockerCertDir string // path to TLS certs for app-01

	// Caddy
	CaddyConfDir      string // path to per-site snippet dir inside Caddy container
	CaddyContainer    string // Docker container name for Caddy
	CaddyStaticVolume string // shared Docker volume name mounted at /srv/sites in Caddy

	// Domain
	BaseDomain string

	// Infrastructure
	AppServerIP           string // IP of the app server (containers + caddy)
	PublicIP              string // Public VPS IP — custom domain A records must point here
	DockerNetwork         string // Docker network for site containers
	CloudflaredConfigPath string // path to cloudflared config.yml
	TunnelName            string // Cloudflare tunnel name
	ServiceTarget         string // upstream service URL for tunnel ingress

	// Worker
	WorkerPollInterval int // seconds
	StuckJobTimeout    int // minutes

	// Backup (R2 / Cloudflare)
	R2AccountID       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string

	// RequireBackupBeforeDestroy gates destroy jobs on a successful pre-destroy
	// backup. Defaults to true. Set REQUIRE_BACKUP_BEFORE_DESTROY=false to
	// skip during development / debugging when R2 is not yet configured.
	RequireBackupBeforeDestroy bool
}

func LoadConfig() Config {
	return Config{
		APIPort:               getEnv("API_PORT", "8080"),
		APIKey:                mustEnv("API_KEY"),
		ControlDSN:            getEnv("CONTROL_DSN", "control:control@123@tcp(10.10.0.20:3306)/controlplane"),
		WordPressDSN:          getEnv("WP_DSN", "control:control@123@tcp(10.10.0.20:3306)/"),
		DockerHost:            getEnv("DOCKER_HOST", "tcp://10.10.0.10:2376"),
		DockerCertDir:         getEnv("DOCKER_CERT_DIR", "/opt/control/certs"),
		CaddyConfDir:          getEnv("CADDY_CONF_DIR", "/etc/caddy/sites"),
		CaddyContainer:        getEnv("CADDY_CONTAINER", "caddy"),
		CaddyStaticVolume:     getEnv("CADDY_STATIC_VOLUME", "caddy_static_sites"),
		BaseDomain:            getEnv("BASE_DOMAIN", "hosto.com"),
		AppServerIP:           getEnv("APP_SERVER_IP", "10.10.0.10"),
		PublicIP:              getEnv("PUBLIC_IP", "129.212.247.213"),
		DockerNetwork:         getEnv("DOCKER_NETWORK", "wp_backend"),
		CloudflaredConfigPath: getEnv("CLOUDFLARED_CONFIG", "/etc/cloudflared/config.yml"),
		TunnelName:            getEnv("TUNNEL_NAME", "hosto"),
		ServiceTarget:         getEnv("TUNNEL_SERVICE_TARGET", "http://10.10.0.10:8080"),
		WorkerPollInterval:    3,
		StuckJobTimeout:       10,
		R2AccountID:                getEnv("R2_ACCOUNT_ID", ""),
		R2AccessKeyID:              getEnv("R2_ACCESS_KEY_ID", ""),
		R2SecretAccessKey:          getEnv("R2_SECRET_ACCESS_KEY", ""),
		R2Bucket:                   getEnv("R2_BUCKET", "hostplane-backups"),
		RequireBackupBeforeDestroy: getEnvBool("REQUIRE_BACKUP_BEFORE_DESTROY", true),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("Required env var %s is not set", key)
	}
	return v
}

func getEnvBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	return v == "true" || v == "1" || v == "yes"
}

func (c Config) DBHost() string {
	// Extract just host:port from the full DSN
	// e.g. "control:pass@tcp(10.10.0.20:3306)/" → "10.10.0.20:3306"
	start := strings.Index(c.WordPressDSN, "tcp(") + 4
	end := strings.Index(c.WordPressDSN, ")/")
	if start < 4 || end < 0 {
		return "10.10.0.20:3306"
	}
	return c.WordPressDSN[start:end]
}
