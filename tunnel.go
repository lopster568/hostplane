package main

import (
    "fmt"
    "os"
    "os/exec"
    "strings"

    "gopkg.in/yaml.v3"
)

const cloudflaredConfig = "/etc/cloudflared/config.yml"

type CloudflaredConfig struct {
    Tunnel          string        `yaml:"tunnel"`
    CredentialsFile string        `yaml:"credentials-file"`
    Ingress         []IngressRule `yaml:"ingress"`
}

type IngressRule struct {
    Hostname string `yaml:"hostname,omitempty"`
    Service  string `yaml:"service"`
}

func loadCloudflaredConfig() (*CloudflaredConfig, error) {
    data, err := os.ReadFile(cloudflaredConfig)
    if err != nil {
        return nil, err
    }
    var cfg CloudflaredConfig
    if err := yaml.Unmarshal(data, &cfg); err != nil {
        return nil, err
    }
    return &cfg, nil
}

func saveCloudflaredConfig(cfg *CloudflaredConfig) error {
    data, err := yaml.Marshal(cfg)
    if err != nil {
        return err
    }
    return os.WriteFile(cloudflaredConfig, data, 0644)
}

func addTunnelRoute(domain string) error {
    cmd := exec.Command("cloudflared", "tunnel", "route", "dns", "hosto", domain)
    out, err := cmd.CombinedOutput()
    if err != nil {
        // Ignore already exists error
        if strings.Contains(string(out), "already exists") {
            return nil
        }
        return fmt.Errorf("cloudflared route dns: %s", string(out))
    }
    return nil
}

func removeTunnelRoute(domain string) error {
    // Cloudflare doesn't have a CLI delete for tunnel DNS routes
    // it must be done via API or manually — we just remove from config
    // The CNAME in customer's DNS will just 404 once removed from ingress
    return nil
}

func updateCloudflaredConfig(domain string) error {
    cfg, err := loadCloudflaredConfig()
    if err != nil {
        return err
    }

    // Check if already exists
    for _, rule := range cfg.Ingress {
        if rule.Hostname == domain {
            return nil
        }
    }

    // Insert before the catch-all (last entry)
    newRule := IngressRule{
        Hostname: domain,
        Service:  "http://10.10.0.10:8080",
    }

    catchAll := cfg.Ingress[len(cfg.Ingress)-1]
    cfg.Ingress = append(cfg.Ingress[:len(cfg.Ingress)-1], newRule, catchAll)

    if err := saveCloudflaredConfig(cfg); err != nil {
        return err
    }

    return restartCloudflared()
}

func removeCloudflaredConfig(domain string) error {
    cfg, err := loadCloudflaredConfig()
    if err != nil {
        return err
    }

    var updated []IngressRule
    for _, rule := range cfg.Ingress {
        if rule.Hostname != domain {
            updated = append(updated, rule)
        }
    }
    cfg.Ingress = updated

    if err := saveCloudflaredConfig(cfg); err != nil {
        return err
    }

    return restartCloudflared()
}

func restartCloudflared() error {
    cmd := exec.Command("systemctl", "restart", "cloudflared")
    out, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("restart cloudflared: %s", string(out))
    }
    return nil
}
