package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
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
	volumeName := StaticVolumeName(site)
	nginxName := StaticContainerName(site)
	domain := SiteDomain(site, p.cfg.BaseDomain)

	// Track what succeeded for rollback
	var volCreated, containerCreated, nginxWritten bool

	rollback := func(reason error) error {
		log.Printf("[rollback] triggered for static %s: %v", site, reason)

		if nginxWritten {
			p.removeNginxConfig(site)
			reloadNginx(p.cfg)
		}
		if containerCreated {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p.docker.ContainerStop(ctx, nginxName, container.StopOptions{})
			p.docker.ContainerRemove(ctx, nginxName, types.ContainerRemoveOptions{Force: true})
		}
		if volCreated {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p.docker.VolumeRemove(ctx, volumeName, true)
		}

		return fmt.Errorf("static provisioning failed (rolled back): %w", reason)
	}

	// Step 1
	if err := p.createVolume(volumeName); err != nil {
		return rollback(fmt.Errorf("createVolume: %w", err))
	}
	volCreated = true

	// Step 2
	if err := p.uploadZipToVolume(volumeName, zipPath); err != nil {
		return rollback(fmt.Errorf("uploadZip: %w", err))
	}

	// Step 3
	if err := p.createContainer(nginxName, volumeName); err != nil {
		return rollback(fmt.Errorf("createContainer: %w", err))
	}
	containerCreated = true

	time.Sleep(2 * time.Second)

	// Step 4
	if err := p.writeNginxConfig(site, nginxName, domain); err != nil {
		return rollback(fmt.Errorf("writeNginxConfig: %w", err))
	}
	nginxWritten = true

	// Step 5
	if err := reloadNginx(p.cfg); err != nil {
		return rollback(fmt.Errorf("reloadNginx: %w", err))
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
				p.cfg.DockerNetwork: {},
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

func (p *StaticProvisioner) removeNginxConfig(site string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	execResp, err := p.docker.ContainerExecCreate(ctx, p.cfg.NginxContainer, types.ExecConfig{
		Cmd: []string{"rm", "-f", "/etc/nginx/conf.d/" + NginxConfFile(site)},
	})
	if err != nil {
		log.Printf("[rollback] cannot create exec for nginx config removal: %v", err)
		return
	}
	p.docker.ContainerExecStart(ctx, execResp.ID, types.ExecStartCheck{})
	log.Printf("[rollback] removed nginx config for static %s", site)
}

func (p *StaticProvisioner) writeNginxConfig(site, nginxName, defaultDomain string, customDomain ...string) error {
	serverName := defaultDomain
	if len(customDomain) > 0 && customDomain[0] != "" {
		serverName = defaultDomain + " " + customDomain[0]
	}

	conf := fmt.Sprintf(`server {
    listen 80;
    server_name %s;

    location / {
        proxy_pass http://%s:80;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
`, serverName, nginxName)

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
