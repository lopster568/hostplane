package main

import (
	"fmt"
	"log"
	"time"
)

type Worker struct {
	db          *DB
	provisioner *Provisioner
	destroyer   *Destroyer
	cfg         Config
}

func NewWorker(db *DB, provisioner *Provisioner, destroyer *Destroyer, cfg Config) *Worker {
	return &Worker{
		db:          db,
		provisioner: provisioner,
		destroyer:   destroyer,
		cfg:         cfg,
	}
}

func (w *Worker) Start() {
	log.Println("[worker] starting")

	// On startup, recover any jobs that were stuck mid-flight
	// when control-01 last crashed or restarted
	recovered, err := w.db.RecoverStuckJobs(w.cfg.StuckJobTimeout)
	if err != nil {
		log.Printf("[worker] stuck job recovery error: %v", err)
	} else if recovered > 0 {
		log.Printf("[worker] recovered %d stuck jobs back to PENDING", recovered)
	}

	ticker := time.NewTicker(time.Duration(w.cfg.WorkerPollInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		w.processNext()
	}
}

func (w *Worker) processNext() {
	job, err := w.db.ClaimNextJob()
	if err != nil {
		log.Printf("[worker] error claiming job: %v", err)
		return
	}
	if job == nil {
		return // nothing pending, silent
	}

	log.Printf("[worker] claimed job %s | type=%s site=%s attempt=%d/%d",
		job.ID, job.Type, job.Site, job.Attempts, job.MaxAttempts)

	var jobErr error

	switch job.Type {
	case JobProvision:
		jobErr = w.provisioner.Run(job.Site)
	case JobDestroy:
		jobErr = w.destroyer.Run(job.Site)
	default:
		jobErr = fmt.Errorf("unknown job type: %s", job.Type)
	}

	if jobErr != nil {
		log.Printf("[worker] job %s FAILED (attempt %d): %v", job.ID, job.Attempts, jobErr)

		// If we've hit max attempts, mark FAILED permanently
		// If not, mark PENDING again so the next poll retries it
		if job.Attempts >= job.MaxAttempts {
			log.Printf("[worker] job %s exhausted all %d attempts, marking FAILED", job.ID, job.MaxAttempts)
			if err := w.db.FailJob(job.ID, job.Site, jobErr); err != nil {
				log.Printf("[worker] error marking job failed: %v", err)
			}
		} else {
			log.Printf("[worker] job %s will retry (%d attempts remaining)", job.ID, job.MaxAttempts-job.Attempts)
			if err := w.db.RetryJob(job.ID, jobErr); err != nil {
				log.Printf("[worker] error scheduling retry: %v", err)
			}
		}
		return
	}

	log.Printf("[worker] job %s COMPLETED | site=%s", job.ID, job.Site)
	if err := w.db.CompleteJob(job.ID, job.Site, job.Type); err != nil {
		log.Printf("[worker] error marking job complete: %v", err)
	}
}
