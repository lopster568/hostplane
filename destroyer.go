package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	_ "github.com/go-sql-driver/mysql"
)

type Destroyer struct {
	docker *client.Client
	cfg    Config
}

func NewDestroyer(docker *client.Client, cfg Config) *Destroyer {
	return &Destroyer{docker: docker, cfg: cfg}
}

func (d *Destroyer) Run(site string) error {
	dbName := WPDatabaseName(site)
	dbUser := WPDatabaseUser(site)
	volumeName := VolumeName(site)
	phpName := PHPContainerName(site)

	if err := d.removeContainer(phpName); err != nil {
		return fmt.Errorf("removeContainer: %w", err)
	}
	if err := d.removeVolume(volumeName); err != nil {
		return fmt.Errorf("removeVolume: %w", err)
	}
	if err := d.removeCaddyConfig(site); err != nil {
		return fmt.Errorf("removeCaddyConfig: %w", err)
	}
	if err := reloadCaddy(d.cfg); err != nil {
		return fmt.Errorf("reloadCaddy: %w", err)
	}
	if err := d.dropDatabase(dbName, dbUser); err != nil {
		return fmt.Errorf("dropDatabase: %w", err)
	}
	return nil
}

func (d *Destroyer) removeContainer(phpName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := d.docker.ContainerRemove(ctx, phpName, types.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: false,
	})
	if err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}

func (d *Destroyer) removeVolume(volumeName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := d.docker.VolumeRemove(ctx, volumeName, true)
	if err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}

func (d *Destroyer) removeCaddyConfig(site string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	confPath := d.cfg.CaddyConfDir + "/" + CaddyConfFile(site)
	execResp, err := d.docker.ContainerExecCreate(ctx, d.cfg.CaddyContainer, types.ExecConfig{
		Cmd: []string{"rm", "-f", confPath},
	})
	if err != nil {
		return err
	}

	return d.docker.ContainerExecStart(ctx, execResp.ID, types.ExecStartCheck{})
}

func (d *Destroyer) dropDatabase(dbName, dbUser string) error {
	db, err := sql.Open("mysql", d.cfg.WordPressDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	if err = db.Ping(); err != nil {
		return fmt.Errorf("cannot reach DB: %w", err)
	}

	stmts := []string{
		"DROP DATABASE IF EXISTS " + dbName,
		fmt.Sprintf("DROP USER IF EXISTS '%s'@'%s'", dbUser, d.cfg.AppServerIP),
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("sql: %w", err)
		}
	}
	return nil
}
