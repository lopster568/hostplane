package main

import (
    "log"
    "os"
    "strings"
)

type Config struct {
    // API
    APIPort   string
    APIKey    string

    // Databases
    ControlDSN string // controlplane DB (jobs, sites)
    WordPressDSN string // root-level DSN to create wp_ databases

    // Docker
    DockerHost    string
    DockerCertDir string // path to TLS certs for app-01

    // Nginx
    NginxConfDir  string
    NginxContainer string

    // Domain
    BaseDomain string

    // Worker
    WorkerPollInterval int // seconds
    StuckJobTimeout    int // minutes
}

func LoadConfig() Config {
    return Config{
        APIPort:            getEnv("API_PORT", "8080"),
        APIKey:             mustEnv("API_KEY"),
        ControlDSN:         getEnv("CONTROL_DSN", "control:control@123@tcp(10.10.0.20:3306)/controlplane"),
        WordPressDSN:       getEnv("WP_DSN", "control:control@123@tcp(10.10.0.20:3306)/"),
        DockerHost:         getEnv("DOCKER_HOST", "tcp://10.10.0.10:2376"),
        DockerCertDir:      getEnv("DOCKER_CERT_DIR", "/opt/control/certs"),
        NginxConfDir:       getEnv("NGINX_CONF_DIR", "/opt/nginx/conf.d"),
        NginxContainer:     getEnv("NGINX_CONTAINER", "edge-nginx"),
        BaseDomain:         getEnv("BASE_DOMAIN", "hosto.com"),
        WorkerPollInterval: 3,
        StuckJobTimeout:    10,
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
