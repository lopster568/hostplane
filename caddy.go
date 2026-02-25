package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

// ensureCaddyConfDir creates the per-site snippet directory inside the Caddy
// container if it does not already exist. CopyToContainer requires the
// destination directory to be present beforehand.
func ensureCaddyConfDir(ctx context.Context, docker *client.Client, containerName, dir string) error {
	execResp, err := docker.ContainerExecCreate(ctx, containerName, types.ExecConfig{
		Cmd: []string{"mkdir", "-p", dir},
	})
	if err != nil {
		return fmt.Errorf("ensureCaddyConfDir: exec create: %w", err)
	}
	if err := docker.ContainerExecStart(ctx, execResp.ID, types.ExecStartCheck{}); err != nil {
		return fmt.Errorf("ensureCaddyConfDir: exec start: %w", err)
	}
	return nil
}

// reloadCaddy signals the Caddy container to reload its configuration.
// It uses `caddy reload` which is a graceful, zero-downtime reload.
func reloadCaddy(cfg Config) error {
	env := append(os.Environ(),
		"DOCKER_HOST="+cfg.DockerHost,
		"DOCKER_TLS_VERIFY=1",
		"DOCKER_CERT_PATH="+cfg.DockerCertDir,
	)

	reload := exec.Command("docker", "exec", cfg.CaddyContainer,
		"caddy", "reload", "--config", "/etc/caddy/Caddyfile")
	reload.Env = env
	if out, err := reload.CombinedOutput(); err != nil {
		return fmt.Errorf("caddy reload failed: %s", string(out))
	}

	return nil
}

// CaddyCertStatus reports whether Caddy has already obtained a TLS certificate
// for a given domain. It execs `caddy list-certificates` inside the Caddy
// container and checks whether the domain appears in the output.
type CaddyCertStatus string

const (
	CertIssued  CaddyCertStatus = "issued"
	CertPending CaddyCertStatus = "pending"
)

// PollCaddyCert polls Caddy's certificate store until the cert for domain is
// issued or the timeout is reached. Returns CertIssued immediately if
// the cert is already present (e.g. renewed from a previous provisioning).
// Returns CertPending if the cert is not yet issued after the timeout — this
// is not an error; Caddy will keep retrying in the background.
func PollCaddyCert(docker *client.Client, cfg Config, domain string, timeout time.Duration) CaddyCertStatus {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if caddyHasCert(docker, cfg, domain) {
			return CertIssued
		}
		time.Sleep(3 * time.Second)
	}
	return CertPending
}

// caddyHasCert execs `caddy list-certificates` in the Caddy container and
// returns true if the domain appears in the output.
func caddyHasCert(docker *client.Client, cfg Config, domain string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	execResp, err := docker.ContainerExecCreate(ctx, cfg.CaddyContainer, types.ExecConfig{
		Cmd:          []string{"caddy", "list-certificates"},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return false
	}

	resp, err := docker.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	if err != nil {
		return false
	}
	defer resp.Close()

	var buf bytes.Buffer
	buf.ReadFrom(resp.Reader)
	return strings.Contains(buf.String(), domain)
}
