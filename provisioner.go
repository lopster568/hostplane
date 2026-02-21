package main

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"fmt"
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
	dbName    := "wp_" + site
	dbUser    := "u_" + site
	dbPass    := "pass_" + site
	volumeName := "vol_" + site
	phpName   := "php_" + site
	domain    := site + "." + p.cfg.BaseDomain

	if err := p.createDatabase(dbName, dbUser, dbPass); err != nil {
		return fmt.Errorf("createDatabase: %w", err)
	}
	if err := p.createVolume(volumeName); err != nil {
		return fmt.Errorf("createVolume: %w", err)
	}
	if err := p.createContainer(phpName, volumeName, dbName, dbUser, dbPass); err != nil {
		return fmt.Errorf("createContainer: %w", err)
	}
	if err := p.writeNginxConfig(site, phpName, domain); err != nil {
		return fmt.Errorf("writeNginxConfig: %w", err)
	}
	if err := reloadNginx(p.cfg); err != nil {
		return fmt.Errorf("reloadNginx: %w", err)
	}
	return nil
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
		fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'10.10.0.10' IDENTIFIED BY '%s'", dbUser, dbPass),
		fmt.Sprintf("GRANT ALL PRIVILEGES ON %s.* TO '%s'@'10.10.0.10'", dbName, dbUser),
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
				"wp_backend": {},
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

func (p *Provisioner) writeNginxConfig(site, phpName, domain string) error {
	conf := fmt.Sprintf(`server {
    listen 80;
    server_name %s;
    root /var/www/html;
    index index.php;

    location / {
        try_files $uri $uri/ /index.php?$args;
    }

    location ~ \.php$ {
        include fastcgi_params;
        fastcgi_pass %s:9000;
        fastcgi_param SCRIPT_FILENAME /var/www/html$fastcgi_script_name;
        fastcgi_param SCRIPT_NAME $fastcgi_script_name;
    }

    location ~ /\.ht {
        deny all;
    }
}
`, domain, phpName)

	// Create a tar archive containing the config file
	// Docker CopyToContainer expects a tar stream
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte(conf)
	err := tw.WriteHeader(&tar.Header{
		Name: site + ".conf",
		Mode: 0644,
		Size: int64(len(content)),
	})
	if err != nil {
		return err
	}
	if _, err := tw.Write(content); err != nil {
		return err
	}
	tw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return p.docker.CopyToContainer(ctx, p.cfg.NginxContainer,
		p.cfg.NginxConfDir,
		&buf,
		types.CopyToContainerOptions{},
	)
}
