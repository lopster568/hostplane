package main

import (
    "database/sql"
    "fmt"
    "time"

    _ "github.com/go-sql-driver/mysql"
)

type JobType   string
type JobStatus string

type Site struct {
	Site      string
	Domain    string
	Status    string
	JobID     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (d *DB) HardDeleteSite(site string) error {
	_, err := d.conn.Exec(`
		DELETE FROM jobs WHERE site=?;
	`, site)
	if err != nil {
		return err
	}
	_, err = d.conn.Exec(`
		DELETE FROM sites WHERE site=?;
	`, site)
	return err
}

func (d *DB) HardDeleteJob(id string) error {
	_, err := d.conn.Exec(`DELETE FROM jobs WHERE id=?`, id)
	return err
}

func (d *DB) ListSites() ([]Site, error) {
	rows, err := d.conn.Query(`
		SELECT site, domain, status, COALESCE(job_id, ''), created_at, updated_at
		FROM sites ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sites []Site
	for rows.Next() {
		var s Site
		if err := rows.Scan(&s.Site, &s.Domain, &s.Status, &s.JobID, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		sites = append(sites, s)
	}
	return sites, nil
}

func (d *DB) GetSite(site string) (*Site, error) {
	var s Site
	err := d.conn.QueryRow(`
		SELECT site, domain, status, COALESCE(job_id, ''), created_at, updated_at
		FROM sites WHERE site=?
	`, site).Scan(&s.Site, &s.Domain, &s.Status, &s.JobID, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

const (
    JobProvision JobType = "PROVISION"
    JobDestroy   JobType = "DESTROY"

    StatusPending    JobStatus = "PENDING"
    StatusProcessing JobStatus = "PROCESSING"
    StatusCompleted  JobStatus = "COMPLETED"
    StatusFailed     JobStatus = "FAILED"
)

type Job struct {
    ID          string
    Type        JobType
    Site        string
    Status      JobStatus
    Attempts    int
    MaxAttempts int
    Error       *string
    CreatedAt   time.Time
    UpdatedAt   time.Time
    StartedAt   *time.Time
    CompletedAt *time.Time
}

type DB struct {
    conn *sql.DB
}

func NewDB(dsn string) (*DB, error) {
    conn, err := sql.Open("mysql", dsn)
    if err != nil {
        return nil, err
    }
    conn.SetMaxOpenConns(10)
    conn.SetMaxIdleConns(5)
    conn.SetConnMaxLifetime(5 * time.Minute)
    if err = conn.Ping(); err != nil {
        return nil, fmt.Errorf("cannot reach control DB: %w", err)
    }
    return &DB{conn: conn}, nil
}

// InsertJob writes a new PENDING job and returns its ID
func (d *DB) InsertJob(id string, jobType JobType, site string) error {
    _, err := d.conn.Exec(`
        INSERT INTO jobs (id, type, site, status, attempts, max_attempts)
        VALUES (?, ?, ?, 'PENDING', 0, 3)
    `, id, jobType, site)
    return err
}

// InsertSite creates or updates the site record
func (d *DB) UpsertSite(site, domain, status, jobID string) error {
    _, err := d.conn.Exec(`
        INSERT INTO sites (site, domain, status, job_id)
        VALUES (?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE status=VALUES(status), job_id=VALUES(job_id), updated_at=NOW()
    `, site, domain, status, jobID)
    return err
}

// UpdateSiteStatus updates just the status of a site
func (d *DB) UpdateSiteStatus(site, status string) error {
    _, err := d.conn.Exec(`
        UPDATE sites SET status=?, updated_at=NOW() WHERE site=?
    `, status, site)
    return err
}

// HasActiveJob checks if site already has a PENDING or PROCESSING job
func (d *DB) HasActiveJob(site string) (bool, error) {
    var count int
    err := d.conn.QueryRow(`
        SELECT COUNT(*) FROM jobs
        WHERE site=? AND status IN ('PENDING','PROCESSING')
    `, site).Scan(&count)
    return count > 0, err
}

// ClaimNextJob atomically claims the next PENDING job using FOR UPDATE SKIP LOCKED
func (d *DB) ClaimNextJob() (*Job, error) {
    tx, err := d.conn.Begin()
    if err != nil {
        return nil, err
    }
    defer tx.Rollback()

    row := tx.QueryRow(`
        SELECT id, type, site, attempts, max_attempts
        FROM jobs
        WHERE status='PENDING' AND attempts < max_attempts
        ORDER BY created_at ASC
        LIMIT 1
        FOR UPDATE SKIP LOCKED
    `)

    var job Job
    err = row.Scan(&job.ID, &job.Type, &job.Site, &job.Attempts, &job.MaxAttempts)
    if err == sql.ErrNoRows {
        return nil, nil // nothing to do
    }
    if err != nil {
        return nil, err
    }

    _, err = tx.Exec(`
        UPDATE jobs
        SET status='PROCESSING', attempts=attempts+1, started_at=NOW(), updated_at=NOW()
        WHERE id=?
    `, job.ID)
    if err != nil {
        return nil, err
    }

    return &job, tx.Commit()
}

// CompleteJob marks job and site as done
func (d *DB) CompleteJob(jobID, site string, jobType JobType) error {
    _, err := d.conn.Exec(`
        UPDATE jobs
        SET status='COMPLETED', completed_at=NOW(), updated_at=NOW(), error=NULL
        WHERE id=?
    `, jobID)
    if err != nil {
        return err
    }

    finalSiteStatus := "ACTIVE"
    if jobType == JobDestroy {
        finalSiteStatus = "DESTROYED"
    }
    return d.UpdateSiteStatus(site, finalSiteStatus)
}

// FailJob marks job as FAILED and stores the error
func (d *DB) FailJob(jobID, site string, jobErr error) error {
    msg := jobErr.Error()
    _, err := d.conn.Exec(`
        UPDATE jobs
        SET status='FAILED', error=?, updated_at=NOW()
        WHERE id=?
    `, msg, jobID)
    if err != nil {
        return err
    }
    return d.UpdateSiteStatus(site, "PROVISIONING") // leave it in limbo, needs manual review
}

// GetJob fetches a job by ID for status polling
func (d *DB) GetJob(id string) (*Job, error) {
    var job Job
    var errStr sql.NullString
    var startedAt, completedAt sql.NullTime

    err := d.conn.QueryRow(`
        SELECT id, type, site, status, attempts, max_attempts, error, created_at, updated_at, started_at, completed_at
        FROM jobs WHERE id=?
    `, id).Scan(
        &job.ID, &job.Type, &job.Site, &job.Status,
        &job.Attempts, &job.MaxAttempts, &errStr,
        &job.CreatedAt, &job.UpdatedAt, &startedAt, &completedAt,
    )
    if err != nil {
        return nil, err
    }

    if errStr.Valid   { job.Error       = &errStr.String  }
    if startedAt.Valid { job.StartedAt  = &startedAt.Time  }
    if completedAt.Valid { job.CompletedAt = &completedAt.Time }

    return &job, nil
}

// RecoverStuckJobs resets PROCESSING jobs that have been running too long (called on startup)
func (d *DB) RecoverStuckJobs(timeoutMinutes int) (int64, error) {
    res, err := d.conn.Exec(`
        UPDATE jobs
        SET status='PENDING', error='recovered: was stuck in PROCESSING', updated_at=NOW()
        WHERE status='PROCESSING'
        AND started_at < NOW() - INTERVAL ? MINUTE
    `, timeoutMinutes)
    if err != nil {
        return 0, err
    }
    return res.RowsAffected()
}

// RetryJob puts a PROCESSING job back to PENDING for the next poll cycle to pick up
func (d *DB) RetryJob(jobID string, jobErr error) error {
	msg := fmt.Sprintf("attempt failed: %s", jobErr.Error())
	_, err := d.conn.Exec(`
		UPDATE jobs
		SET status='PENDING', error=?, updated_at=NOW()
		WHERE id=?
	`, msg, jobID)
	return err
}
