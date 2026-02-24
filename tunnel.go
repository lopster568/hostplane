package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"
)

// TunnelManager manages Cloudflare tunnel configuration and DNS routes.
// All methods use configurable paths from Config — no hard-coded constants.
type TunnelManager struct {
	cfg Config
}

func NewTunnelManager(cfg Config) *TunnelManager {
	return &TunnelManager{cfg: cfg}
}

type CloudflaredConfig struct {
	Tunnel          string        `yaml:"tunnel"`
	CredentialsFile string        `yaml:"credentials-file"`
	Ingress         []IngressRule `yaml:"ingress"`
}

type IngressRule struct {
	Hostname string `yaml:"hostname,omitempty"`
	Service  string `yaml:"service"`
}

func (tm *TunnelManager) loadConfig() (*CloudflaredConfig, error) {
	data, err := os.ReadFile(tm.cfg.CloudflaredConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read cloudflared config %s: %w", tm.cfg.CloudflaredConfigPath, err)
	}
	var cfg CloudflaredConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse cloudflared config: %w", err)
	}
	return &cfg, nil
}

// saveConfig writes the cloudflared config atomically using yaml.Marshal.
// Writes to a temp file first, then renames to avoid partial writes.
func (tm *TunnelManager) saveConfig(cfg *CloudflaredConfig) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("marshal cloudflared config: %w", err)
	}
	enc.Close()

	tmpPath := tm.cfg.CloudflaredConfigPath + ".tmp"
	if err := os.WriteFile(tmpPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("write temp config %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, tm.cfg.CloudflaredConfigPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}

// AddRoute creates a DNS CNAME for the domain pointing to the tunnel.
// Idempotent — returns nil if the route already exists.
func (tm *TunnelManager) AddRoute(domain string) error {
	cmd := exec.Command("cloudflared", "tunnel", "route", "dns", tm.cfg.TunnelName, domain)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "already exists") {
			log.Printf("[tunnel] DNS route for %s already exists, skipping", domain)
			return nil
		}
		return fmt.Errorf("cloudflared route dns: %s", string(out))
	}
	log.Printf("[tunnel] added DNS route for %s", domain)
	return nil
}

// RemoveRoute removes a DNS route. Currently a no-op because the cloudflared CLI
// does not support route deletion — requires Cloudflare API.
func (tm *TunnelManager) RemoveRoute(domain string) error {
	log.Printf("[tunnel] DNS route removal for %s requires manual Cloudflare API call", domain)
	return nil
}

// UpdateConfig adds a domain to the cloudflared ingress and restarts synchronously.
// Idempotent — skips if the domain is already present.
func (tm *TunnelManager) UpdateConfig(domain string) error {
	cfg, err := tm.loadConfig()
	if err != nil {
		return err
	}

	// Idempotent check
	for _, rule := range cfg.Ingress {
		if rule.Hostname == domain {
			log.Printf("[tunnel] domain %s already in config, skipping", domain)
			return nil
		}
	}

	newRule := IngressRule{
		Hostname: domain,
		Service:  tm.cfg.ServiceTarget,
	}

	// Insert before catch-all (last entry)
	if len(cfg.Ingress) > 0 {
		catchAll := cfg.Ingress[len(cfg.Ingress)-1]
		cfg.Ingress = append(cfg.Ingress[:len(cfg.Ingress)-1], newRule, catchAll)
	} else {
		cfg.Ingress = append(cfg.Ingress, newRule, IngressRule{Service: "http_status:404"})
	}

	if err := tm.saveConfig(cfg); err != nil {
		return err
	}

	log.Printf("[tunnel] added %s to config, restarting cloudflared", domain)
	return restartCloudflared()
}

// RemoveConfig removes a domain from the cloudflared ingress and restarts synchronously.
// Idempotent — no-op if the domain is not present.
func (tm *TunnelManager) RemoveConfig(domain string) error {
	cfg, err := tm.loadConfig()
	if err != nil {
		return err
	}

	var updated []IngressRule
	found := false
	for _, rule := range cfg.Ingress {
		if rule.Hostname != domain {
			updated = append(updated, rule)
		} else {
			found = true
		}
	}

	if !found {
		log.Printf("[tunnel] domain %s not in config, nothing to remove", domain)
		return nil
	}

	cfg.Ingress = updated

	if err := tm.saveConfig(cfg); err != nil {
		return err
	}

	log.Printf("[tunnel] removed %s from config, restarting cloudflared", domain)
	return restartCloudflared()
}

func restartCloudflared() error {
	cmd := exec.Command("systemctl", "restart", "cloudflared")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart cloudflared: %s", string(out))
	}
	log.Println("[tunnel] cloudflared restarted successfully")
	return nil
}
