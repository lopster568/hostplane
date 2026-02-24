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
	"github.com/docker/docker/client"
)

type StaticProvisioner struct {
	docker *client.Client
	cfg    Config
}

func NewStaticProvisioner(docker *client.Client, cfg Config) *StaticProvisioner {
	return &StaticProvisioner{docker: docker, cfg: cfg}
}

// Run provisions a static site.
// Files from the uploaded zip are extracted into the shared caddy_static_sites
// volume under /{site}/, then a Caddy snippet is written and Caddy is reloaded.
// No per-site container is created — Caddy's file_server handles serving directly.
func (p *StaticProvisioner) Run(site, zipPath string) error {
	domain := SiteDomain(site, p.cfg.BaseDomain)

	var filesUploaded, caddyWritten bool

	rollback := func(reason error) error {
		log.Printf("[rollback] triggered for static %s: %v", site, reason)

		if caddyWritten {
			p.removeCaddyConfig(site)
			reloadCaddy(p.cfg)
		}
		if filesUploaded {
			p.removeStaticSiteFiles(site)
		}

		return fmt.Errorf("static provisioning failed (rolled back): %w", reason)
	}

	// Step 1: extract zip into caddy_static_sites volume under /{site}/
	if err := p.uploadZipToStaticSites(site, zipPath); err != nil {
		return rollback(fmt.Errorf("uploadZip: %w", err))
	}
	filesUploaded = true

	// Step 2: write Caddy snippet that serves /srv/sites/{site} via file_server
	if err := p.writeCaddyConfig(site, domain); err != nil {
		return rollback(fmt.Errorf("writeCaddyConfig: %w", err))
	}
	caddyWritten = true

	// Step 3: reload Caddy
	if err := reloadCaddy(p.cfg); err != nil {
		return rollback(fmt.Errorf("reloadCaddy: %w", err))
	}

	os.Remove(zipPath)
	return nil
}

// uploadZipToStaticSites extracts the zip into the shared caddy_static_sites
// Docker volume under the site's own subdirectory (/{site}/).
// It uses a temporary busybox container to perform the copy.
func (p *StaticProvisioner) uploadZipToStaticSites(site, zipPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tmpName := "tmp_static_" + site

	resp, err := p.docker.ContainerCreate(
		ctx,
		&container.Config{Image: "busybox", Cmd: []string{"sh"}},
		&container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: p.cfg.CaddyStaticVolume,
					Target: "/data",
				},
			},
		},
		nil, nil, tmpName,
	)
	if err != nil {
		return fmt.Errorf("create temp container: %w", err)
	}
	defer p.docker.ContainerRemove(ctx, resp.ID, types.ContainerRemoveOptions{Force: true})

	tarBuf, err := zipToTar(zipPath)
	if err != nil {
		return fmt.Errorf("zip to tar: %w", err)
	}

	// Copy files into /data/{site}/ — this becomes /srv/sites/{site} inside Caddy
	destPath := "/data/" + site
	if err = p.docker.CopyToContainer(ctx, resp.ID, destPath, tarBuf, types.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}

	return nil
}

// removeStaticSiteFiles deletes /data/{site}/ from the shared caddy_static_sites
// volume using a temporary busybox container.
func (p *StaticProvisioner) removeStaticSiteFiles(site string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmpName := "tmp_rmstatic_" + site

	resp, err := p.docker.ContainerCreate(
		ctx,
		&container.Config{
			Image: "busybox",
			Cmd:   []string{"rm", "-rf", "/data/" + site},
		},
		&container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: p.cfg.CaddyStaticVolume,
					Target: "/data",
				},
			},
		},
		nil, nil, tmpName,
	)
	if err != nil {
		log.Printf("[rollback] cannot create rm container for static %s: %v", site, err)
		return
	}
	defer p.docker.ContainerRemove(ctx, resp.ID, types.ContainerRemoveOptions{Force: true})

	p.docker.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})

	statusCh, errCh := p.docker.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			log.Printf("[rollback] wait error removing static files for %s: %v", site, err)
		}
	case <-statusCh:
	}
	log.Printf("[rollback] removed static site files for %s", site)
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

// writeCaddyConfig writes a Caddy snippet that serves the static site via
// file_server. The Caddy container must have caddy_static_sites mounted at
// /srv/sites, so each site's files live at /srv/sites/{site}/.
func (p *StaticProvisioner) writeCaddyConfig(site, defaultDomain string, customDomain ...string) error {
	hosts := defaultDomain
	if len(customDomain) > 0 && customDomain[0] != "" {
		hosts = defaultDomain + ", " + customDomain[0]
	}

	conf := fmt.Sprintf(`%s {
    root * /srv/sites/%s
    file_server
    encode gzip
}
`, hosts, site)

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

// removeCaddyConfig removes the per-site Caddy snippet from the Caddy container.
func (p *StaticProvisioner) removeCaddyConfig(site string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	confPath := p.cfg.CaddyConfDir + "/" + CaddyConfFile(site)
	execResp, err := p.docker.ContainerExecCreate(ctx, p.cfg.CaddyContainer, types.ExecConfig{
		Cmd: []string{"rm", "-f", confPath},
	})
	if err != nil {
		log.Printf("[rollback] cannot create exec for caddy config removal (static %s): %v", site, err)
		return
	}
	p.docker.ContainerExecStart(ctx, execResp.ID, types.ExecStartCheck{})
	log.Printf("[rollback] removed caddy config for static %s", site)
}
