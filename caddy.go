package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

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
