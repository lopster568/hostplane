package main

import (
	"database/sql"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var validSite = regexp.MustCompile(`^[a-z0-9]+$`)

type API struct {
	db  *DB
	cfg Config
}

func NewAPI(db *DB, cfg Config) *API {
	return &API{db: db, cfg: cfg}
}

// POST /api/static/provision
func (a *API) handleStaticProvision(c *gin.Context) {
	site := strings.ToLower(c.PostForm("site"))
	if site == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "site is required"})
		return
	}
	if !validSite.MatchString(site) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "site name must be lowercase letters and numbers only"})
		return
	}

	file, err := c.FormFile("zip")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "zip file is required"})
		return
	}

	// Save zip temporarily
	tmpPath := "/tmp/" + site + ".zip"
	if err := c.SaveUploadedFile(file, tmpPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save zip"})
		return
	}

	jobID := uuid.New().String()
	domain := site + "." + a.cfg.BaseDomain

	if err := a.db.InsertJob(jobID, JobStaticProvision, site); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to queue job"})
		return
	}

	if err := a.db.UpsertSite(site, domain, "PROVISIONING", jobID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record site"})
		return
	}

	// Store zip path in job payload so worker can find it
	if err := a.db.SetJobPayload(jobID, tmpPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store payload"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"job_id": jobID,
		"site":   site,
		"domain": domain,
		"status": "PENDING",
	})
}

func (a *API) RegisterRoutes(r *gin.Engine) {
	r.Use(a.authMiddleware())

	v1 := r.Group("/api")
	{
		v1.POST("/provision", a.handleProvision)
		v1.POST("/destroy",   a.handleDestroy)
		v1.GET("/jobs/:id",   a.handleJobStatus)
		v1.GET("/sites/:site", a.handleSiteStatus)
		v1.GET("/health",     a.handleHealth)
		v1.GET("/sites", a.handleListSites)	
		v1.DELETE("/sites/:site", a.handleDeleteSite)
		v1.DELETE("/jobs/:id",    a.handleDeleteJob)
		v1.POST("/static/provision", a.handleStaticProvision)
	}
}

// POST /api/provision
func (a *API) handleProvision(c *gin.Context) {
	var req struct {
		Site string `json:"site" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "site is required"})
		return
	}

	site := strings.ToLower(req.Site)

	if !validSite.MatchString(site) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "site name must be lowercase letters and numbers only"})
		return
	}

	// Reject if site already has an active job
	active, err := a.db.HasActiveJob(site)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check job status"})
		return
	}
	if active {
		c.JSON(http.StatusConflict, gin.H{"error": "site already has a pending or processing job"})
		return
	}

	// Reject if site is already active
	existing, err := a.db.GetSite(site)
	if err != nil && err != sql.ErrNoRows {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check site status"})
		return
	}
	if existing != nil && existing.Status == "ACTIVE" {
		c.JSON(http.StatusConflict, gin.H{"error": "site already exists and is active"})
		return
	}

	jobID := uuid.New().String()
	domain := site + "." + a.cfg.BaseDomain

	if err := a.db.InsertJob(jobID, JobProvision, site); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to queue job"})
		return
	}

	if err := a.db.UpsertSite(site, domain, "PROVISIONING", jobID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record site"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"job_id": jobID,
		"site":   site,
		"domain": domain,
		"status": "PENDING",
	})
}

// GET /api/sites
func (a *API) handleListSites(c *gin.Context) {
	sites, err := a.db.ListSites()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch sites"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sites": sites})
}

// DELETE /api/sites/:site
func (a *API) handleDeleteSite(c *gin.Context) {
	site := c.Param("site")

	existing, err := a.db.GetSite(site)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "site not found"})
		return
	}
	if existing.Status != "DESTROYED" {
		c.JSON(http.StatusConflict, gin.H{"error": "site must be DESTROYED before hard delete"})
		return
	}

	if err := a.db.HardDeleteSite(site); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete site"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"deleted": site})
}

// DELETE /api/jobs/:id
func (a *API) handleDeleteJob(c *gin.Context) {
	id := c.Param("id")

	job, err := a.db.GetJob(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	if job.Status == StatusProcessing || job.Status == StatusPending {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot delete active job"})
		return
	}

	if err := a.db.HardDeleteJob(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete job"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"deleted": id})
}

// POST /api/destroy
func (a *API) handleDestroy(c *gin.Context) {
	var req struct {
		Site string `json:"site" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "site is required"})
		return
	}

	site := strings.ToLower(req.Site)

	if !validSite.MatchString(site) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "site name must be lowercase letters and numbers only"})
		return
	}

	// Must exist and not already be destroying
	existing, err := a.db.GetSite(site)
	if err == sql.ErrNoRows || existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "site not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check site"})
		return
	}
	if existing.Status == "DESTROYING" || existing.Status == "DESTROYED" {
		c.JSON(http.StatusConflict, gin.H{"error": "site is already being destroyed or is destroyed"})
		return
	}

	// Reject if already has active job
	active, err := a.db.HasActiveJob(site)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check job status"})
		return
	}
	if active {
		c.JSON(http.StatusConflict, gin.H{"error": "site already has a pending or processing job"})
		return
	}

	jobID := uuid.New().String()

	if err := a.db.InsertJob(jobID, JobDestroy, site); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to queue job"})
		return
	}

	if err := a.db.UpsertSite(site, existing.Domain, "DESTROYING", jobID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update site status"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"job_id": jobID,
		"site":   site,
		"status": "PENDING",
	})
}

// GET /api/jobs/:id
func (a *API) handleJobStatus(c *gin.Context) {
	id := c.Param("id")

	job, err := a.db.GetJob(id)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"job_id":       job.ID,
		"type":         job.Type,
		"site":         job.Site,
		"status":       job.Status,
		"attempts":     job.Attempts,
		"max_attempts": job.MaxAttempts,
		"error":        job.Error,
		"created_at":   job.CreatedAt,
		"started_at":   job.StartedAt,
		"completed_at": job.CompletedAt,
	})
}

// GET /api/sites/:site
func (a *API) handleSiteStatus(c *gin.Context) {
	site := c.Param("site")

	s, err := a.db.GetSite(site)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "site not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch site"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"site":       s.Site,
		"domain":     s.Domain,
		"status":     s.Status,
		"job_id":     s.JobID,
		"created_at": s.CreatedAt,
		"updated_at": s.UpdatedAt,
	})
}

// GET /api/health
func (a *API) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// authMiddleware validates the X-API-Key header on every request
func (a *API) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Health check bypasses auth
		if c.Request.URL.Path == "/api/health" {
			c.Next()
			return
		}

		key := c.GetHeader("X-API-Key")
		if key == "" || key != a.cfg.APIKey {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		c.Next()
	}
}
