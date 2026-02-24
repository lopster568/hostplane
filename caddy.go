package main

import (
	"fmt"
	"os"
	"os/exec"
)

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
