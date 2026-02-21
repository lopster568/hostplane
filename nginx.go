package main

import (
	"fmt"
	"os"
	"os/exec"
)

func reloadNginx(cfg Config) error {
	env := append(os.Environ(),
		"DOCKER_HOST="+cfg.DockerHost,
		"DOCKER_TLS_VERIFY=1",
		"DOCKER_CERT_PATH="+cfg.DockerCertDir,
	)

	test := exec.Command("docker", "exec", cfg.NginxContainer, "nginx", "-t")
	test.Env = env
	if out, err := test.CombinedOutput(); err != nil {
		return fmt.Errorf("nginx test failed: %s", string(out))
	}

	reload := exec.Command("docker", "exec", cfg.NginxContainer, "nginx", "-s", "reload")
	reload.Env = env
	if out, err := reload.CombinedOutput(); err != nil {
		return fmt.Errorf("nginx reload failed: %s", string(out))
	}

	return nil
}
