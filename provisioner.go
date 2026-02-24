package main

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	_ "github.com/go-sql-driver/mysql"
)

type Provisioner struct {
	docker *client.Client
	cfg    Config
}

func NewProvisioner(docker *client.Client, cfg Config) *Provisioner {
	return &Provisioner{docker: docker, cfg: cfg}
}

func (p *Provisioner) Run(site string) error {
	dbName := WPDatabaseName(site)
	dbUser := WPDatabaseUser(site)
	dbPass := WPDatabasePass(site)
	volName := VolumeName(site)
	phpName := PHPContainerName(site)
	domain := SiteDomain(site, p.cfg.BaseDomain)

	// Track what succeeded for rollback
	var dbCreated, volCreated, containerCreated, caddyWritten bool

	// Rollback function — runs in reverse order
	rollback := func(reason error) error {
		log.Printf("[rollback] triggered for %s: %v", site, reason)

		if caddyWritten {
			p.removeCaddyConfig(site)
			reloadCaddy(p.cfg)
		}
		if containerCreated {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p.docker.ContainerStop(ctx, phpName, container.StopOptions{})
			p.docker.ContainerRemove(ctx, phpName, types.ContainerRemoveOptions{Force: true})
		}
		if volCreated {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p.docker.VolumeRemove(ctx, volName, true)
		}
		if dbCreated {
			p.dropDatabase(dbName, dbUser)
		}

		return fmt.Errorf("provisioning failed (rolled back): %w", reason)
	}

	// Step 1
	if err := p.createDatabase(dbName, dbUser, dbPass); err != nil {
		return rollback(fmt.Errorf("createDatabase: %w", err))
	}
	dbCreated = true

	// Step 2
	if err := p.createVolume(volName); err != nil {
		return rollback(fmt.Errorf("createVolume: %w", err))
	}
	volCreated = true

	// Step 3
	if err := p.createContainer(phpName, volName, dbName, dbUser, dbPass); err != nil {
		return rollback(fmt.Errorf("createContainer: %w", err))
	}
	containerCreated = true

	// Step 4
	if err := p.writeCaddyConfig(site, phpName, domain); err != nil {
		return rollback(fmt.Errorf("writeCaddyConfig: %w", err))
	}
	caddyWritten = true

	// Step 5
	if err := reloadCaddy(p.cfg); err != nil {
		return rollback(fmt.Errorf("reloadCaddy: %w", err))
	}

	return nil
}

func (p *Provisioner) dropDatabase(dbName, dbUser string) {
	db, err := sql.Open("mysql", p.cfg.WordPressDSN)
	if err != nil {
		log.Printf("[rollback] cannot open DB connection: %v", err)
		return
	}
	defer db.Close()
	db.Exec("DROP DATABASE IF EXISTS " + dbName)
	db.Exec("DROP USER IF EXISTS '" + dbUser + "'@'" + p.cfg.AppServerIP + "'")
	log.Printf("[rollback] dropped DB %s and user %s", dbName, dbUser)
}

func (p *Provisioner) removeCaddyConfig(site string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	confPath := p.cfg.CaddyConfDir + "/" + CaddyConfFile(site)
	execResp, err := p.docker.ContainerExecCreate(ctx, p.cfg.CaddyContainer, types.ExecConfig{
		Cmd: []string{"rm", "-f", confPath},
	})
	if err != nil {
		log.Printf("[rollback] cannot create exec for caddy config removal: %v", err)
		return
	}
	p.docker.ContainerExecStart(ctx, execResp.ID, types.ExecStartCheck{})
	log.Printf("[rollback] removed caddy config for %s", site)
}

func (p *Provisioner) createDatabase(dbName, dbUser, dbPass string) error {
	db, err := sql.Open("mysql", p.cfg.WordPressDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	if err = db.Ping(); err != nil {
		return fmt.Errorf("cannot reach DB: %w", err)
	}

	stmts := []string{
		"CREATE DATABASE IF NOT EXISTS " + dbName,
		fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%s' IDENTIFIED BY '%s'", dbUser, p.cfg.AppServerIP, dbPass),
		fmt.Sprintf("GRANT ALL PRIVILEGES ON %s.* TO '%s'@'%s'", dbName, dbUser, p.cfg.AppServerIP),
		"FLUSH PRIVILEGES",
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("sql(%s): %w", stmt[:20], err)
		}
	}
	return nil
}

func (p *Provisioner) createVolume(volumeName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := p.docker.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName})
	return err
}

func (p *Provisioner) createContainer(phpName, volumeName, dbName, dbUser, dbPass string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Idempotent — container already exists, just ensure it's running
	_, err := p.docker.ContainerInspect(ctx, phpName)
	if err == nil {
		return p.docker.ContainerStart(ctx, phpName, types.ContainerStartOptions{})
	}

	pids := int64(100)

	resp, err := p.docker.ContainerCreate(
		ctx,
		&container.Config{
			Image: "wordpress:php8.2-fpm",
			Env: []string{
				"WORDPRESS_DB_HOST=" + p.cfg.DBHost(),
				"WORDPRESS_DB_USER=" + dbUser,
				"WORDPRESS_DB_PASSWORD=" + dbPass,
				"WORDPRESS_DB_NAME=" + dbName,
			},
		},
		&container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: volumeName,
					Target: "/var/www/html",
				},
			},
			Resources: container.Resources{
				Memory:    512 * 1024 * 1024,
				NanoCPUs:  1_000_000_000,
				PidsLimit: &pids,
			},
		},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				p.cfg.DockerNetwork: {},
			},
		},
		nil,
		phpName,
	)
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}

	return p.docker.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
}

// writeCaddyConfig writes a per-site Caddy snippet into the CaddyConfDir inside
// the Caddy container. WordPress containers speak PHP-FPM (port 9000), so we
// use the php_fastcgi directive. The root directive sets SCRIPT_FILENAME for
// PHP-FPM without requiring Caddy to have direct file access.
func (p *Provisioner) writeCaddyConfig(site, phpName, defaultDomain string, customDomain ...string) error {
	hosts := defaultDomain
	if len(customDomain) > 0 && customDomain[0] != "" {
		hosts = defaultDomain + ", " + customDomain[0]
	}

	conf := fmt.Sprintf(`%s {
    root * /var/www/html
    php_fastcgi %s:9000
    file_server
    encode gzip
}
`, hosts, phpName)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte(conf)
	tw.WriteHeader(&tar.Header{
		Name:    CaddyConfFile(site),
		Mode:    0644,
		Size:    int64(len(content)),
		ModTime: time.Now(),
	})
	tw.Write(content)
	tw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ensureCaddyConfDir(ctx, p.docker, p.cfg.CaddyContainer, p.cfg.CaddyConfDir); err != nil {
		return err
	}

	return p.docker.CopyToContainer(ctx, p.cfg.CaddyContainer,
		p.cfg.CaddyConfDir, &buf, types.CopyToContainerOptions{})
}
