package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

type StaticProvisioner struct {
	docker *client.Client
	cfg    Config
}

func NewStaticProvisioner(docker *client.Client, cfg Config) *StaticProvisioner {
	return &StaticProvisioner{docker: docker, cfg: cfg}
}

func (p *StaticProvisioner) Run(site, zipPath string) error {
	volumeName  := "static_vol_" + site
	nginxName   := "static_" + site
	domain      := site + "." + p.cfg.BaseDomain

	if err := p.createVolume(volumeName); err != nil {
		return fmt.Errorf("createVolume: %w", err)
	}

	if err := p.uploadZipToVolume(volumeName, zipPath); err != nil {
		return fmt.Errorf("uploadZip: %w", err)
	}

	if err := p.createContainer(nginxName, volumeName); err != nil {
		return fmt.Errorf("createContainer: %w", err)
	}

	time.Sleep(2 * time.Second)

	if err := p.writeNginxConfig(site, nginxName, domain); err != nil {
		return fmt.Errorf("writeNginxConfig: %w", err)
	}

	if err := reloadNginx(p.cfg); err != nil {
		return fmt.Errorf("reloadNginx: %w", err)
	}

	// Cleanup tmp zip
	os.Remove(zipPath)

	return nil
}

func (p *StaticProvisioner) createVolume(volumeName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := p.docker.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName})
	return err
}

func (p *StaticProvisioner) uploadZipToVolume(volumeName, zipPath string) error {
	// Extract zip and build tar stream to copy into a temp container
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create a temporary container to copy files into the volume
	resp, err := p.docker.ContainerCreate(
		ctx,
		&container.Config{Image: "busybox", Cmd: []string{"sh"}},
		&container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: volumeName,
					Target: "/data",
				},
			},
		},
		nil, nil, "tmp_upload_"+volumeName,
	)
	if err != nil {
		return fmt.Errorf("create temp container: %w", err)
	}

	defer p.docker.ContainerRemove(ctx, resp.ID, types.ContainerRemoveOptions{Force: true})

	// Build tar from zip contents
	tarBuf, err := zipToTar(zipPath)
	if err != nil {
		return fmt.Errorf("zip to tar: %w", err)
	}

	// Copy tar into container at /data
	err = p.docker.CopyToContainer(ctx, resp.ID, "/data", tarBuf, types.CopyToContainerOptions{})
	if err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}

	return nil
}

func zipToTar(zipPath string) (io.Reader, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return nil, err
		}

		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}

		// Strip top-level directory from zip if present
		name := filepath.Base(f.Name)
		if filepath.Dir(f.Name) != "." {
			name = f.Name
		}

		tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		})
		tw.Write(content)
	}

	tw.Close()
	return &buf, nil
}

func (p *StaticProvisioner) createContainer(nginxName, volumeName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Idempotent
	_, err := p.docker.ContainerInspect(ctx, nginxName)
	if err == nil {
		return p.docker.ContainerStart(ctx, nginxName, types.ContainerStartOptions{})
	}

	resp, err := p.docker.ContainerCreate(
		ctx,
		&container.Config{
			Image: "nginx:stable",
		},
		&container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: volumeName,
					Target: "/usr/share/nginx/html",
				},
			},
		},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				"wp_backend": {},
			},
		},
		nil,
		nginxName,
	)
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}

	return p.docker.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
}

func (p *StaticProvisioner) writeNginxConfig(site, nginxName, domain string) error {
	conf := fmt.Sprintf(`server {
    listen 80;
    server_name %s;
    root /usr/share/nginx/html;
    index index.html;

    location / {
        try_files $uri $uri/ /index.html;
    }

    location ~ /\.ht {
        deny all;
    }
}
`, domain, nginxName)

	// Note: static sites use nginx container directly, no PHP
	// We proxy to the static nginx container
	conf = fmt.Sprintf(`server {
    listen 80;
    server_name %s;

    location / {
        proxy_pass http://%s:80;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
`, domain, nginxName)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte(conf)
	tw.WriteHeader(&tar.Header{
		Name: site + ".conf",
		Mode: 0644,
		Size: int64(len(content)),
	})
	tw.Write(content)
	tw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return p.docker.CopyToContainer(ctx, p.cfg.NginxContainer,
		p.cfg.NginxConfDir, &buf, types.CopyToContainerOptions{})
}
